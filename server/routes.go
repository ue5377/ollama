package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/envconfig"
	"github.com/ollama/ollama/gpu"
	"github.com/ollama/ollama/llm"
	"github.com/ollama/ollama/openai"
	"github.com/ollama/ollama/parser"
	"github.com/ollama/ollama/template"
	"github.com/ollama/ollama/types/errtypes"
	"github.com/ollama/ollama/types/model"
	"github.com/ollama/ollama/version"
)

var mode string = gin.DebugMode

type Server struct {
	addr  net.Addr
	sched *Scheduler
}

func init() {
	switch mode {
	case gin.DebugMode:
	case gin.ReleaseMode:
	case gin.TestMode:
	default:
		mode = gin.DebugMode
	}

	gin.SetMode(mode)
}

var defaultSessionDuration = 5 * time.Minute

func modelOptions(model *Model, requestOpts map[string]interface{}) (api.Options, error) {
	opts := api.DefaultOptions()
	if err := opts.FromMap(model.Options); err != nil {
		return api.Options{}, err
	}

	if err := opts.FromMap(requestOpts); err != nil {
		return api.Options{}, err
	}

	return opts, nil
}

func (s *Server) scheduleCompletion(ctx context.Context, name string, requestOpts map[string]any, keepAlive *api.Duration) (*runnerRef, error) {
	if name == "" {
		return nil, errors.New("model is required")
	}

	model, err := GetModel(name)
	if err != nil {
		return nil, err
	}

	if !model.Can(CapCompletion) {
		return nil, errors.New("model does not support completion")
	}

	opts, err := modelOptions(model, requestOpts)
	if err != nil {
		return nil, err
	}

	sessionDuration := getDefaultSessionDuration()
	if keepAlive != nil {
		sessionDuration = keepAlive.Duration
	}

	runnerCh, errCh := s.sched.GetRunner(ctx, model, opts, sessionDuration)
	var runner *runnerRef
	select {
	case runner = <-runnerCh:
	case err = <-errCh:
		return nil, err
	}

	return runner, nil
}

func (s *Server) GenerateHandler(c *gin.Context) {
	var req api.GenerateRequest
	if err := c.ShouldBindJSON(&req); errors.Is(err, io.EOF) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	} else if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Format != "" && req.Format != "json" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "format must be empty or \"json\""})
		return
	} else if req.Raw && (req.Template != "" || req.System != "" || len(req.Context) > 0) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "raw mode does not support template, system, or context"})
		return
	}

	r, err := s.scheduleCompletion(c.Request.Context(), req.Model, req.Options, req.KeepAlive)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	images := make([]llm.ImageData, len(req.Images))
	for i := range req.Images {
		images[i] = llm.ImageData{ID: i, Data: req.Images[i]}
	}

	prompt := req.Prompt
	if !req.Raw {
		var msgs []api.Message
		if req.System != "" {
			msgs = append(msgs, api.Message{Role: "system", Content: req.System})
		} else if r.model.System != "" {
			msgs = append(msgs, api.Message{Role: "system", Content: r.model.System})
		}

		if req.Prompt != "" {
			for _, i := range images {
				msgs = append(msgs, api.Message{Role: "user", Content: fmt.Sprintf("[img-%d]", i.ID)})
			}

			msgs = append(msgs, api.Message{Role: "user", Content: req.Prompt})
		}

		if len(msgs) == 0 {
			c.JSON(http.StatusOK, api.GenerateResponse{
				Model:      req.Model,
				CreatedAt:  time.Now().UTC(),
				Done:       true,
				DoneReason: "load",
			})
			return
		}

		tmpl := r.model.Template
		if req.Template != "" {
			tmpl, err = template.Parse(req.Template)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}

		var b bytes.Buffer
		if req.Context != nil {
			s, err := r.llama.Detokenize(c.Request.Context(), req.Context)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			b.WriteString(s)
		}

		if err := tmpl.Execute(&b, template.Values{Messages: msgs}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		prompt = b.String()
	}

	slog.Debug("generate request", "prompt", prompt, "images", images)

	ch := make(chan any)
	go func() {
		defer close(ch)
		if err := r.llama.Completion(c.Request.Context(), llm.CompletionRequest{
			Prompt:  prompt,
			Images:  images,
			Format:  req.Format,
			Options: *r.Options,
		}, func(r llm.CompletionResponse) {
			ch <- api.GenerateResponse{
				Model:      req.Model,
				CreatedAt:  time.Now().UTC(),
				Response:   r.Content,
				Done:       r.Done,
				DoneReason: r.DoneReason,
				Metrics: api.Metrics{
					PromptEvalCount:    r.PromptEvalCount,
					PromptEvalDuration: r.PromptEvalDuration,
					EvalCount:          r.EvalCount,
					EvalDuration:       r.EvalDuration,
				},
			}
		}); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		var r api.GenerateResponse
		var sb strings.Builder
		for rr := range ch {
			switch t := rr.(type) {
			case api.GenerateResponse:
				sb.WriteString(t.Response)
				r = t
			case gin.H:
				msg, ok := t["error"].(string)
				if !ok {
					msg = "unexpected error format in response"
				}

				c.JSON(http.StatusInternalServerError, gin.H{"error": msg})
				return
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected response"})
				return
			}
		}

		r.Response = sb.String()
		c.JSON(http.StatusOK, r)
		return
	}

	streamResponse(c, ch)
}

func getDefaultSessionDuration() time.Duration {
	if envconfig.KeepAlive != "" {
		v, err := strconv.Atoi(envconfig.KeepAlive)
		if err != nil {
			d, err := time.ParseDuration(envconfig.KeepAlive)
			if err != nil {
				return defaultSessionDuration
			}

			if d < 0 {
				return time.Duration(math.MaxInt64)
			}

			return d
		}

		d := time.Duration(v) * time.Second
		if d < 0 {
			return time.Duration(math.MaxInt64)
		}
		return d
	}

	return defaultSessionDuration
}

func (s *Server) EmbeddingsHandler(c *gin.Context) {
	var req api.EmbeddingRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Model == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	model, err := GetModel(req.Model)
	if err != nil {
		var pErr *fs.PathError
		if errors.As(err, &pErr) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found, try pulling it first", req.Model)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	opts, err := modelOptions(model, req.Options)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var sessionDuration time.Duration
	if req.KeepAlive == nil {
		sessionDuration = getDefaultSessionDuration()
	} else {
		sessionDuration = req.KeepAlive.Duration
	}

	rCh, eCh := s.sched.GetRunner(c.Request.Context(), model, opts, sessionDuration)
	var runner *runnerRef
	select {
	case runner = <-rCh:
	case err = <-eCh:
		handleErrorResponse(c, err)
		return
	}

	// an empty request loads the model
	if req.Prompt == "" {
		c.JSON(http.StatusOK, api.EmbeddingResponse{Embedding: []float64{}})
		return
	}

	embedding, err := runner.llama.Embedding(c.Request.Context(), req.Prompt)
	if err != nil {
		slog.Info(fmt.Sprintf("embedding generation failed: %v", err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate embedding"})
		return
	}

	resp := api.EmbeddingResponse{
		Embedding: embedding,
	}
	c.JSON(http.StatusOK, resp)
}

func (s *Server) PullModelHandler(c *gin.Context) {
	var req api.PullRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	name := model.ParseName(cmp.Or(req.Model, req.Name))
	if !name.IsValid() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid model name"})
		return
	}

	if err := checkNameExists(name); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.ProgressResponse) {
			ch <- r
		}

		regOpts := &registryOptions{
			Insecure: req.Insecure,
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		if err := PullModel(ctx, name.DisplayShortest(), regOpts, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func (s *Server) PushModelHandler(c *gin.Context) {
	var req api.PushRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var model string
	if req.Model != "" {
		model = req.Model
	} else if req.Name != "" {
		model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.ProgressResponse) {
			ch <- r
		}

		regOpts := &registryOptions{
			Insecure: req.Insecure,
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		if err := PushModel(ctx, model, regOpts, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func checkNameExists(name model.Name) error {
	names, err := Manifests()
	if err != nil {
		return err
	}

	for n := range names {
		if strings.EqualFold(n.Filepath(), name.Filepath()) && n != name {
			return fmt.Errorf("a model with that name already exists")
		}
	}

	return nil
}

func (s *Server) CreateModelHandler(c *gin.Context) {
	var r api.CreateRequest
	if err := c.ShouldBindJSON(&r); errors.Is(err, io.EOF) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	} else if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	name := model.ParseName(cmp.Or(r.Model, r.Name))
	if !name.IsValid() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": errtypes.InvalidModelNameErrMsg})
		return
	}

	if err := checkNameExists(name); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if r.Path == "" && r.Modelfile == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "path or modelfile are required"})
		return
	}

	var sr io.Reader = strings.NewReader(r.Modelfile)
	if r.Path != "" && r.Modelfile == "" {
		f, err := os.Open(r.Path)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("error reading modelfile: %s", err)})
			return
		}
		defer f.Close()

		sr = f
	}

	f, err := parser.ParseFile(sr)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(resp api.ProgressResponse) {
			ch <- resp
		}

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		quantization := cmp.Or(r.Quantize, r.Quantization)
		if err := CreateModel(ctx, name, filepath.Dir(r.Path), strings.ToUpper(quantization), f, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if r.Stream != nil && !*r.Stream {
		waitForStream(c, ch)
		return
	}

	streamResponse(c, ch)
}

func (s *Server) DeleteModelHandler(c *gin.Context) {
	var r api.DeleteRequest
	if err := c.ShouldBindJSON(&r); errors.Is(err, io.EOF) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	} else if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	n := model.ParseName(cmp.Or(r.Model, r.Name))
	if !n.IsValid() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("name %q is invalid", cmp.Or(r.Model, r.Name))})
		return
	}

	m, err := ParseNamedManifest(n)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if err := m.Remove(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if err := m.RemoveLayers(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
}

func (s *Server) ShowModelHandler(c *gin.Context) {
	var req api.ShowRequest
	err := c.ShouldBindJSON(&req)
	switch {
	case errors.Is(err, io.EOF):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Model != "" {
		// noop
	} else if req.Name != "" {
		req.Model = req.Name
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	resp, err := GetModelInfo(req)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Model)})
		case err.Error() == "invalid model name":
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, resp)
}

func GetModelInfo(req api.ShowRequest) (*api.ShowResponse, error) {
	m, err := GetModel(req.Model)
	if err != nil {
		return nil, err
	}

	modelDetails := api.ModelDetails{
		ParentModel:       m.ParentModel,
		Format:            m.Config.ModelFormat,
		Family:            m.Config.ModelFamily,
		Families:          m.Config.ModelFamilies,
		ParameterSize:     m.Config.ModelType,
		QuantizationLevel: m.Config.FileType,
	}

	if req.System != "" {
		m.System = req.System
	}

	if req.Template != "" {
		m.Template, err = template.Parse(req.Template)
		if err != nil {
			return nil, err
		}
	}

	msgs := make([]api.Message, len(m.Messages))
	for i, msg := range m.Messages {
		msgs[i] = api.Message{Role: msg.Role, Content: msg.Content}
	}

	n := model.ParseName(req.Model)
	if !n.IsValid() {
		return nil, fmt.Errorf("invalid model name")
	}

	manifest, err := ParseNamedManifest(n)
	if err != nil {
		return nil, err
	}

	resp := &api.ShowResponse{
		License:    strings.Join(m.License, "\n"),
		System:     m.System,
		Template:   m.Template.String(),
		Details:    modelDetails,
		Messages:   msgs,
		ModifiedAt: manifest.fi.ModTime(),
	}

	var params []string
	cs := 30
	for k, v := range m.Options {
		switch val := v.(type) {
		case []interface{}:
			for _, nv := range val {
				params = append(params, fmt.Sprintf("%-*s %#v", cs, k, nv))
			}
		default:
			params = append(params, fmt.Sprintf("%-*s %#v", cs, k, v))
		}
	}
	resp.Parameters = strings.Join(params, "\n")

	for k, v := range req.Options {
		if _, ok := req.Options[k]; ok {
			m.Options[k] = v
		}
	}

	var sb strings.Builder
	fmt.Fprintln(&sb, "# Modelfile generated by \"ollama show\"")
	fmt.Fprintln(&sb, "# To build a new Modelfile based on this, replace FROM with:")
	fmt.Fprintf(&sb, "# FROM %s\n\n", m.ShortName)
	fmt.Fprint(&sb, m.String())
	resp.Modelfile = sb.String()

	return resp, nil
}

func (s *Server) ListModelsHandler(c *gin.Context) {
	ms, err := Manifests()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	models := []api.ListModelResponse{}
	for n, m := range ms {
		f, err := m.Config.Open()
		if err != nil {
			slog.Warn("bad manifest filepath", "name", n, "error", err)
			continue
		}
		defer f.Close()

		var cf ConfigV2
		if err := json.NewDecoder(f).Decode(&cf); err != nil {
			slog.Warn("bad manifest config", "name", n, "error", err)
			continue
		}

		// tag should never be masked
		models = append(models, api.ListModelResponse{
			Model:      n.DisplayShortest(),
			Name:       n.DisplayShortest(),
			Size:       m.Size(),
			Digest:     m.digest,
			ModifiedAt: m.fi.ModTime(),
			Details: api.ModelDetails{
				Format:            cf.ModelFormat,
				Family:            cf.ModelFamily,
				Families:          cf.ModelFamilies,
				ParameterSize:     cf.ModelType,
				QuantizationLevel: cf.FileType,
			},
		})
	}

	slices.SortStableFunc(models, func(i, j api.ListModelResponse) int {
		// most recently modified first
		return cmp.Compare(j.ModifiedAt.Unix(), i.ModifiedAt.Unix())
	})

	c.JSON(http.StatusOK, api.ListResponse{Models: models})
}

func (s *Server) CopyModelHandler(c *gin.Context) {
	var r api.CopyRequest
	if err := c.ShouldBindJSON(&r); errors.Is(err, io.EOF) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	} else if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	src := model.ParseName(r.Source)
	if !src.IsValid() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("source %q is invalid", r.Source)})
		return
	}

	dst := model.ParseName(r.Destination)
	if !dst.IsValid() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("destination %q is invalid", r.Destination)})
		return
	}

	if err := checkNameExists(dst); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := CopyModel(src, dst); errors.Is(err, os.ErrNotExist) {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model %q not found", r.Source)})
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func (s *Server) HeadBlobHandler(c *gin.Context) {
	path, err := GetBlobsPath(c.Param("digest"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if _, err := os.Stat(path); err != nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("blob %q not found", c.Param("digest"))})
		return
	}

	c.Status(http.StatusOK)
}

func (s *Server) CreateBlobHandler(c *gin.Context) {
	if ib, ok := intermediateBlobs[c.Param("digest")]; ok {
		p, err := GetBlobsPath(ib)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			slog.Info("evicting intermediate blob which no longer exists", "digest", ib)
			delete(intermediateBlobs, c.Param("digest"))
		} else if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		} else {
			c.Status(http.StatusOK)
			return
		}
	}

	path, err := GetBlobsPath(c.Param("digest"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	_, err = os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// noop
	case err != nil:
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	default:
		c.Status(http.StatusOK)
		return
	}

	layer, err := NewLayer(c.Request.Body, "")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if layer.Digest != c.Param("digest") {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("digest mismatch, expected %q, got %q", c.Param("digest"), layer.Digest)})
		return
	}

	c.Status(http.StatusCreated)
}

func isLocalIP(ip netip.Addr) bool {
	if interfaces, err := net.Interfaces(); err == nil {
		for _, iface := range interfaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}

			for _, a := range addrs {
				if parsed, _, err := net.ParseCIDR(a.String()); err == nil {
					if parsed.String() == ip.String() {
						return true
					}
				}
			}
		}
	}

	return false
}

func allowedHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}

	if hostname, err := os.Hostname(); err == nil && host == hostname {
		return true
	}

	var tlds = []string{
		"localhost",
		"local",
		"internal",
	}

	// check if the host is a local TLD
	for _, tld := range tlds {
		if strings.HasSuffix(host, "."+tld) {
			return true
		}
	}

	return false
}

func allowedHostsMiddleware(addr net.Addr) gin.HandlerFunc {
	return func(c *gin.Context) {
		if addr == nil {
			c.Next()
			return
		}

		if addr, err := netip.ParseAddrPort(addr.String()); err == nil && !addr.Addr().IsLoopback() {
			c.Next()
			return
		}

		host, _, err := net.SplitHostPort(c.Request.Host)
		if err != nil {
			host = c.Request.Host
		}

		if addr, err := netip.ParseAddr(host); err == nil {
			if addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() || isLocalIP(addr) {
				c.Next()
				return
			}
		}

		if allowedHost(host) {
			if c.Request.Method == http.MethodOptions {
				c.AbortWithStatus(http.StatusNoContent)
				return
			}

			c.Next()
			return
		}

		c.AbortWithStatus(http.StatusForbidden)
	}
}

func (s *Server) GenerateRoutes() http.Handler {
	config := cors.DefaultConfig()
	config.AllowWildcard = true
	config.AllowBrowserExtensions = true
	config.AllowHeaders = []string{"Authorization", "Content-Type", "User-Agent", "Accept", "X-Requested-With"}
	openAIProperties := []string{"lang", "package-version", "os", "arch", "runtime", "runtime-version", "async"}
	for _, prop := range openAIProperties {
		config.AllowHeaders = append(config.AllowHeaders, "x-stainless-"+prop)
	}
	config.AllowOrigins = envconfig.AllowOrigins

	r := gin.Default()
	r.Use(
		cors.New(config),
		allowedHostsMiddleware(s.addr),
	)

	r.POST("/api/pull", s.PullModelHandler)
	r.POST("/api/generate", s.GenerateHandler)
	r.POST("/api/chat", s.ChatHandler)
	r.POST("/api/embeddings", s.EmbeddingsHandler)
	r.POST("/api/create", s.CreateModelHandler)
	r.POST("/api/push", s.PushModelHandler)
	r.POST("/api/copy", s.CopyModelHandler)
	r.DELETE("/api/delete", s.DeleteModelHandler)
	r.POST("/api/show", s.ShowModelHandler)
	r.POST("/api/blobs/:digest", s.CreateBlobHandler)
	r.HEAD("/api/blobs/:digest", s.HeadBlobHandler)
	r.GET("/api/ps", s.ProcessHandler)

	// Compatibility endpoints
	r.POST("/v1/chat/completions", openai.Middleware(), s.ChatHandler)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r.Handle(method, "/", func(c *gin.Context) {
			c.String(http.StatusOK, "Ollama is running")
		})

		r.Handle(method, "/api/tags", s.ListModelsHandler)
		r.Handle(method, "/api/version", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"version": version.Version})
		})
	}

	return r
}

func Serve(ln net.Listener) error {
	level := slog.LevelInfo
	if envconfig.Debug {
		level = slog.LevelDebug
	}

	slog.Info("server config", "env", envconfig.Values())
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.SourceKey {
				source := attr.Value.Any().(*slog.Source)
				source.File = filepath.Base(source.File)
			}

			return attr
		},
	})

	slog.SetDefault(slog.New(handler))

	blobsDir, err := GetBlobsPath("")
	if err != nil {
		return err
	}
	if err := fixBlobs(blobsDir); err != nil {
		return err
	}

	if !envconfig.NoPrune {
		// clean up unused layers and manifests
		if err := PruneLayers(); err != nil {
			return err
		}

		manifestsPath, err := GetManifestPath()
		if err != nil {
			return err
		}

		if err := PruneDirectory(manifestsPath); err != nil {
			return err
		}
	}

	ctx, done := context.WithCancel(context.Background())
	schedCtx, schedDone := context.WithCancel(ctx)
	sched := InitScheduler(schedCtx)
	s := &Server{addr: ln.Addr(), sched: sched}
	r := s.GenerateRoutes()

	slog.Info(fmt.Sprintf("Listening on %s (version %s)", ln.Addr(), version.Version))
	srvr := &http.Server{
		Handler: r,
	}

	// listen for a ctrl+c and stop any loaded llm
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		srvr.Close()
		schedDone()
		sched.unloadAllRunners()
		gpu.Cleanup()
		done()
	}()

	if err := llm.Init(); err != nil {
		return fmt.Errorf("unable to initialize llm library %w", err)
	}

	s.sched.Run(schedCtx)

	// At startup we retrieve GPU information so we can get log messages before loading a model
	// This will log warnings to the log in case we have problems with detected GPUs
	gpus := gpu.GetGPUInfo()
	gpus.LogDetails()

	err = srvr.Serve(ln)
	// If server is closed from the signal handler, wait for the ctx to be done
	// otherwise error out quickly
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-ctx.Done()
	return nil
}

func waitForStream(c *gin.Context, ch chan interface{}) {
	c.Header("Content-Type", "application/json")
	for resp := range ch {
		switch r := resp.(type) {
		case api.ProgressResponse:
			if r.Status == "success" {
				c.JSON(http.StatusOK, r)
				return
			}
		case gin.H:
			if errorMsg, ok := r["error"].(string); ok {
				c.JSON(http.StatusInternalServerError, gin.H{"error": errorMsg})
				return
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected error format in progress response"})
				return
			}
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected progress response"})
			return
		}
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected end of progress response"})
}

func streamResponse(c *gin.Context, ch chan any) {
	c.Header("Content-Type", "application/x-ndjson")
	c.Stream(func(w io.Writer) bool {
		val, ok := <-ch
		if !ok {
			return false
		}

		bts, err := json.Marshal(val)
		if err != nil {
			slog.Info(fmt.Sprintf("streamResponse: json.Marshal failed with %s", err))
			return false
		}

		// Delineate chunks with new-line delimiter
		bts = append(bts, '\n')
		if _, err := w.Write(bts); err != nil {
			slog.Info(fmt.Sprintf("streamResponse: w.Write failed with %s", err))
			return false
		}

		return true
	})
}

func (s *Server) ProcessHandler(c *gin.Context) {
	models := []api.ProcessModelResponse{}

	for _, v := range s.sched.loaded {
		model := v.model
		modelDetails := api.ModelDetails{
			Format:            model.Config.ModelFormat,
			Family:            model.Config.ModelFamily,
			Families:          model.Config.ModelFamilies,
			ParameterSize:     model.Config.ModelType,
			QuantizationLevel: model.Config.FileType,
		}

		mr := api.ProcessModelResponse{
			Model:     model.ShortName,
			Name:      model.ShortName,
			Size:      int64(v.estimatedTotal),
			SizeVRAM:  int64(v.estimatedVRAM),
			Digest:    model.Digest,
			Details:   modelDetails,
			ExpiresAt: v.expiresAt,
		}
		// The scheduler waits to set expiresAt, so if a model is loading it's
		// possible that it will be set to the unix epoch. For those cases, just
		// calculate the time w/ the sessionDuration instead.
		var epoch time.Time
		if v.expiresAt == epoch {
			mr.ExpiresAt = time.Now().Add(v.sessionDuration)
		}

		models = append(models, mr)
	}

	c.JSON(http.StatusOK, api.ProcessResponse{Models: models})
}

func (s *Server) ChatHandler(c *gin.Context) {
	var req api.ChatRequest
	if err := c.ShouldBindJSON(&req); errors.Is(err, io.EOF) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing request body"})
		return
	} else if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	r, err := s.scheduleCompletion(c.Request.Context(), req.Model, req.Options, req.KeepAlive)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if len(req.Messages) == 0 {
		c.JSON(http.StatusOK, api.ChatResponse{
			Model:      req.Model,
			CreatedAt:  time.Now().UTC(),
			Message:    api.Message{Role: "assistant"},
			Done:       true,
			DoneReason: "load",
		})
		return
	}

	prompt, images, err := chatPrompt(c.Request.Context(), r, req.Messages)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	slog.Debug("chat request", "images", len(images), "prompt", prompt)

	ch := make(chan any)
	go func() {
		defer close(ch)
		if err := r.llama.Completion(c.Request.Context(), llm.CompletionRequest{
			Prompt:  prompt,
			Images:  images,
			Format:  req.Format,
			Options: *r.Options,
		}, func(r llm.CompletionResponse) {
			ch <- api.ChatResponse{
				Model:      req.Model,
				CreatedAt:  time.Now().UTC(),
				Message:    api.Message{Role: "assistant", Content: r.Content},
				Done:       r.Done,
				DoneReason: r.DoneReason,
				Metrics: api.Metrics{
					PromptEvalCount:    r.PromptEvalCount,
					PromptEvalDuration: r.PromptEvalDuration,
					EvalCount:          r.EvalCount,
					EvalDuration:       r.EvalDuration,
				},
			}
		}); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	if req.Stream != nil && !*req.Stream {
		var r api.ChatResponse
		var sb strings.Builder
		for rr := range ch {
			switch t := rr.(type) {
			case api.ChatResponse:
				sb.WriteString(t.Message.Content)
				r = t
			case gin.H:
				msg, ok := t["error"].(string)
				if !ok {
					msg = "unexpected error format in response"
				}

				c.JSON(http.StatusInternalServerError, gin.H{"error": msg})
				return
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected response"})
				return
			}
		}

		r.Message.Content = sb.String()
		c.JSON(http.StatusOK, r)
		return
	}

	streamResponse(c, ch)
}

func handleErrorResponse(c *gin.Context, err error) {
	if errors.Is(err, context.Canceled) {
		c.JSON(499, gin.H{"error": "request canceled"})
		return
	}
	if errors.Is(err, ErrMaxQueue) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}
