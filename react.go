package mono

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/goccy/go-yaml"
	"github.com/gomarkdown/markdown"
	mdhtml "github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
	"golang.org/x/net/html"
	xhtml "html"
	"html/template"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func React(dir string, config ...ReactConfig) Endpoint {
	cfg := def(config, ReactConfig{})
	base := maps.Clone(LibReactBaseFuncMap)
	maps.Insert(base, maps.All(cfg.FuncMap))
	cfg.FuncMap = base
	return &implReact{
		Dir:    dir,
		Config: cfg,
		prefix: fmt.Sprintf("/_monotonic_%d/", implReactIndex.Add(1)),
	}
}

// todo: support this
var implReactIndex *atomic.Int64 = &atomic.Int64{}

type TailwindConfig struct {
	Path     []string
	Timeout  time.Duration
	ConfigJs []byte
	InputCss []byte
	Enabled  bool
}

var Tailwind = TailwindConfig{
	Path: []string{"npx", "@tailwindcss/cli"},
	// Timeout is huge, for npx to be able to update the tailwind cli, if its stale on your machine.
	Timeout:  time.Minute,
	ConfigJs: tailwindConfigJs,
	InputCss: tailwindInputCss,
	Enabled:  true,
}

var (
	//go:embed static/tailwind.config.js
	tailwindConfigJs []byte

	//go:embed static/tailwind.css
	tailwindInputCss []byte
)

type ReactConfig struct {
	FuncMap  template.FuncMap
	Handlers map[string]HandlerFunc
}

// todo: support this for real
//
//go:embed static/minihtmx.js
var LibReactJs string

var LibReactBaseFuncMap template.FuncMap = map[string]interface{}{
	"echo": func(str string) string { return str },
	"time": func() string { return time.Now().Format("15:04:05") },
	"script_inline": func(filename string) (template.HTML, error) {
		data, err := os.ReadFile(filename)
		if err != nil {
			return "", err
		}
		return template.HTML(fmt.Sprintf("<script>%s</script>", string(data))), nil
	},
	"minihtmx": func() template.HTML {
		return template.HTML(fmt.Sprintf("<script>%s</script>", LibReactJs))
	},
	"base":  filepath.Base,
	"join":  strings.Join,
	"split": strings.Split,
	"trim":  strings.Trim,
	"sub": func(a, b any) any {
		switch v := a.(type) {
		case int:
			return v - b.(int)
		case int64:
			return v - b.(int64)
		case float64:
			return v - b.(float64)
		default:
			return nil
		}
	},
	"auto_dark_mode": func(args ...string) template.HTML {
		mode := "system"
		if len(args) > 0 && args[0] != "" {
			mode = args[0]
		}

		var condition string
		switch mode {
		case "dark":
			condition = "true"
		case "light":
			condition = "false"
		default:
			condition = `localStorage.theme === 'dark' || ((!('theme' in localStorage) || localStorage.theme === 'system') && window.matchMedia('(prefers-color-scheme: dark)').matches)`
		}

		return template.HTML(fmt.Sprintf(`<script>
try {
    const is_dark = %s;
    document.documentElement.classList.toggle('dark', is_dark);
    if (is_dark) {
        document.querySelector('meta[name="theme-color"]')?.setAttribute('content', '#09090b');
    }
} catch (_) {}
</script>`, condition))
	},
	"dict": func(args ...any) (map[string]any, error) {
		if len(args)%2 != 0 {
			return nil, fmt.Errorf("dict expects even number of arguments, got %d", len(args))
		}
		result := make(map[string]any, len(args)/2)
		for i := 0; i < len(args); i += 2 {
			key, ok := args[i].(string)
			if !ok {
				return nil, fmt.Errorf("dict expects string keys, got %T", args[i])
			}
			result[key] = args[i+1]
		}
		return result, nil
	},
	"list": func(args ...any) []any { return args },
}

type implReact struct {
	Dir    string
	Config ReactConfig
	prefix string
}

type implReactBuildResult struct {
	Result   map[string]*html.Node
	Handlers map[string]HandlerFunc
	Error    error
}

func (react *implReact) Endpoint(endpoints Endpoints) error {
	resultChannel := make(chan implReactBuildResult)
	react.Config.FuncMap["file_src"] = filesHost(resultChannel, react.prefix)

	go func() {
		defer close(resultChannel)
		react.Build(react.Dir, resultChannel, nil)
	}()

	pages := map[string]*html.Node{}
	handlers := map[string]HandlerFunc{}
	errs := make([]error, 0)
	for result := range resultChannel {
		if result.Error != nil {
			errs = append(errs, result.Error)
		}
		if result.Result != nil {
			maps.Copy(pages, result.Result)
		}
		if result.Handlers != nil {
			maps.Copy(handlers, result.Handlers)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("react build: %v", err)
	}

	if Tailwind.Enabled {
		if err := react.buildTailwind(pages); err != nil {
			return fmt.Errorf("react build tailwind: %v", err)
		}
	}

	for k, v := range pages {
		var buff bytes.Buffer
		if err := html.Render(&buff, v); err != nil {
			return fmt.Errorf("react render %s: %v", k, err)
		}
		endpoints[k] = func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			if _, err := rw.Write(buff.Bytes()); err != nil {
				return err
			}
			return nil
		}
	}
	maps.Copy(endpoints, handlers)
	return nil
}

func filesHost(result chan implReactBuildResult, prefix string) func(filename string, contentTypeHint ...string) (template.URL, error) {
	fileSrcRegistered := map[string]bool{}
	fileSrcRegisteredLock := &sync.Mutex{}
	cache := s3fifoNew(80)
	return func(filename string, contentTypeHint ...string) (template.URL, error) {
		fileSrcRegisteredLock.Lock()
		defer fileSrcRegisteredLock.Unlock()
		data, err := os.ReadFile(filename)
		if err != nil {
			return "", fmt.Errorf("read file %s: %w", filename, err)
		}
		link := filepath.Join(prefix, fmt.Sprintf("%s%s", hash(data), filepath.Ext(filename)))
		if _, ok := fileSrcRegistered[link]; ok {
			return template.URL(link), nil
		}
		fileSrcRegistered[link] = true

		contentType := http.DetectContentType(data)
		if len(contentTypeHint) > 0 {
			contentType = contentTypeHint[0]
		}

		result <- implReactBuildResult{
			Handlers: map[string]HandlerFunc{
				link: reactMediaHandler(cache, link, filename, contentType),
			},
		}
		return template.URL(link), nil
	}
}

func reactMediaHandler(cache *s3fifo, link string, filename string, contentType string) func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	return func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
		rw.Header().Set("Content-Type", contentType)
		media, ok := cache.Get(link)
		if ok {
			if _, err := rw.Write(media); err != nil {
				return err
			}
			return nil
		}

		stat, err := os.Stat(filename)
		if err != nil {
			return err
		}
		if stat.Size() >= (50 << 20) {
			file, err := os.Open(filename)
			if err != nil {
				return err
			}
			defer file.Close()
			if _, err := io.Copy(rw, file); err != nil {
				return err
			}
			return nil
		}

		media, err = os.ReadFile(filename)
		if err != nil {
			return err
		}
		cache.Set(link, media)
		if _, err := rw.Write(media); err != nil {
			return err
		}

		return nil
	}
}

type buildDirectoryPath struct {
	Name  string
	IsDir bool
	Info  os.FileInfo
}

func (react *implReact) Build(path string, result chan implReactBuildResult, layout *template.Template) {
	info, err := os.Stat(path)
	if err != nil {
		result <- implReactBuildResult{Error: err}
		return
	}

	var subpaths []buildDirectoryPath
	if info.IsDir() {
		var elements []os.DirEntry
		elements, err = os.ReadDir(path)
		for _, el := range elements {
			elInfo, err := el.Info()
			if err != nil {
				result <- implReactBuildResult{Error: err}
				return
			}
			subpaths = append(subpaths, buildDirectoryPath{
				Name:  elInfo.Name(),
				IsDir: elInfo.IsDir(),
				Info:  elInfo,
			})
		}
	} else {
		subpaths = append(subpaths, buildDirectoryPath{
			Name:  info.Name(),
			IsDir: info.IsDir(),
			Info:  info,
		})
	}
	if err != nil {
		result <- implReactBuildResult{Error: err}
		return
	}
	wg := sync.WaitGroup{}
	endpoints, newLayout, err := react.buildDirectory(path, subpaths, layout)
	if err != nil {
		result <- implReactBuildResult{Error: fmt.Errorf("build dir %s: %w", path, err)}
	}
	if endpoints != nil {
		result <- implReactBuildResult{Result: endpoints}
	}
	if newLayout != nil {
		layout = newLayout
	}
	for _, subpath := range subpaths {
		if subpath.IsDir {
			var branchLayout *template.Template
			if layout != nil {
				var err error
				branchLayout, err = layout.Clone()
				if err != nil {
					result <- implReactBuildResult{Error: fmt.Errorf("clone layout: %w", err)}
					continue
				}
			}
			wg.Go(func() {
				react.Build(filepath.Join(path, subpath.Name), result, branchLayout)
			})
		}
	}
	wg.Wait()
}

func (react *implReact) buildDirectory(
	root string, paths []buildDirectoryPath, layout *template.Template,
) (
	result map[string]*html.Node, newLayout *template.Template, err error,
) {
	funcs := maps.Clone(react.Config.FuncMap)
	funcs["pwd"] = func() string { return root }
	funcs["rel"] = func() (string, error) { return filepath.Rel(react.Dir, root) }
	funcs["url"] = func() (string, error) {
		rel, err := filepath.Rel(react.Dir, root)
		if err != nil {
			return "", err
		}
		return filepath.Join("/", rel), nil
	}
	funcs["absolute"] = func(filename string) (string, error) { return filepath.Abs(filepath.Join(root, filename)) }
	funcs["funcs"] = func() template.FuncMap { return funcs }

	files := map[string][]byte{}
	containsIndexFile := false
	for _, path := range paths {
		if path.IsDir {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, path.Name))
		if err != nil {
			return nil, nil, fmt.Errorf("error reading %s: %w", path.Name, err)
		}
		files[path.Name] = data

		if strings.HasPrefix(path.Name, "index.") {
			containsIndexFile = true
		}
	}

	page := &bytes.Buffer{}
	if data, ok := files["index.html"]; ok {
		page = bytes.NewBuffer(data)
	}
	if page, err = react.buildDirectoryIndexGohtmlOpt(files, funcs, page); err != nil {
		return nil, nil, err
	}
	if page, err = react.buildDirectoryIndexMdOpt(files, funcs, page); err != nil {
		return nil, nil, err
	}
	if layout, err = react.buildDirectoryLayoutYamlOpt(files, funcs, layout, root); err != nil {
		return nil, nil, err
	}
	if !containsIndexFile {
		return nil, layout, nil
	}
	if page, layout, err = react.buildDirectoryFinal(files, funcs, page, layout); err != nil {
		return nil, nil, err
	}

	parsed, err := html.Parse(page)
	if err != nil {
		return nil, nil, err
	}

	urlPath, err := filepath.Rel(react.Dir, root)
	if err != nil {
		return nil, nil, fmt.Errorf("error computing relative path: %w (%s -> %s)", err, react.Dir, root)
	}
	return map[string]*html.Node{urlPath: parsed}, layout, nil
}

func (react *implReact) buildDirectoryIndexGohtmlOpt(files map[string][]byte, funcs template.FuncMap, page *bytes.Buffer) (*bytes.Buffer, error) {
	data, ok := files["index.gohtml"]
	if !ok {
		return page, nil
	}

	templ, err := template.New("index.gohtml").
		Funcs(funcs).
		Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("error parsing index.gohtml: %w", err)
	}
	if err := templ.Execute(page, nil); err != nil {
		return nil, fmt.Errorf("error executing index.gohtml: %w", err)
	}
	return page, nil
}

func (react *implReact) buildDirectoryFinal(files map[string][]byte, funcs template.FuncMap, page *bytes.Buffer, layout *template.Template) (pageWithLayout *bytes.Buffer, newLayout *template.Template, err error) {
	if layout == nil {
		return page, nil, nil
	}

	layout.
		Funcs(funcs).
		Funcs(makeBodyFunc(page))

	pageWithLayout = &bytes.Buffer{}
	if err := templCloneExecute(layout, pageWithLayout, nil); err != nil {
		return nil, nil, fmt.Errorf("error executing layout.gohtml: %w", err)
	}
	return pageWithLayout, layout, nil
}

func (react *implReact) buildDirectoryLayoutYamlOpt(files map[string][]byte, funcs template.FuncMap, layout *template.Template, root string) (newLayout *template.Template, err error) {
	data, ok := files["layout.yaml"]
	if !ok {
		return layout, nil
	}

	var lay reactYamlLayout
	if err := yaml.Unmarshal(data, &lay); err != nil {
		return nil, fmt.Errorf("error unmarshalling layout.yaml: %w", err)
	}

	dynamicComponents := map[string]string{}
	if len(lay.Components) > 0 {
		funcs = maps.Clone(funcs)
		for name, component := range lay.Components {
			file, err := os.ReadFile(filepath.Join(root, component.Source))
			if err != nil {
				return nil, fmt.Errorf("error reading %s: %w", component.Source, err)
			}
			if component.Dynamic {
				dynamicComponents[name] = string(file)
				continue
			}
			funcs[name] = func() (template.HTML, error) {
				templ, err := template.New(name).
					Funcs(funcs).
					Parse(string(file))
				if err != nil {
					return "", fmt.Errorf("error parsing %s: %w", name, err)
				}
				buff := &bytes.Buffer{}
				if err := templ.Execute(buff, nil); err != nil {
					return "", fmt.Errorf("error executing %s: %w", name, err)
				}
				return template.HTML(buff.String()), nil
			}
		}
	}

	head := []string{
		fmt.Sprintf(`<title>%s</title>`, xhtmlEscapeString(lay.Title)),
	}
	if lay.Icon != "" {
		head = append(head,
			fmt.Sprintf(`<link rel="icon" type="image/png" sizes="32x32" href="{{file_src "%s"}}" crossorigin="anonymous">`,
				filepath.Join(root, lay.Icon),
			),
		)
	}
	for _, value := range lay.Head {
		head = append(head, value)
	}
	for name, value := range lay.Meta {
		head = append(head, fmt.Sprintf(`<meta name="%s" content="%s">`,
			xhtmlEscapeString(name),
			xhtmlEscapeString(value),
		))
	}
	bodyStart := []string{}
	bodyEnd := []string{}
	for _, value := range lay.Body {
		if len(value.List) == 0 {
			bodyStart = append(bodyStart, value.String)
		}
		if len(value.List) > 0 {
			bodyStart = append(bodyStart, value.List[0])
		}
		if len(value.List) > 1 {
			bodyEnd = append(bodyEnd, value.List[1])
		}
	}
	slices.Reverse(bodyEnd)

	schema := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	%s
</head>
<body>
	%s
	{{body}}
	%s
</body>
</html>
`,
		strings.Join(head, "\n"),
		strings.Join(bodyStart, "\n"),
		strings.Join(bodyEnd, "\n"),
	)
	for name, value := range dynamicComponents {
		schema = strings.ReplaceAll(schema, fmt.Sprintf("{{%s}}", name), value)
	}

	templ, err := template.New("layout.gohtml").
		Funcs(funcs).
		Funcs(makeBodyFunc(&bytes.Buffer{})).
		Parse(schema)
	if err != nil {
		return nil, fmt.Errorf("error parsing layout.yaml: %w", err)
	}
	return templ, nil
}

func (react *implReact) buildTailwind(pages map[string]*html.Node) error {
	dir, err := os.MkdirTemp("", "monotonic-tailwind-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := react.writePagesToDir(dir, pages); err != nil {
		return fmt.Errorf("write pages: %w", err)
	}

	files := map[string][]byte{
		"tailwind.config.js": Tailwind.ConfigJs,
		"input.css":          tailwindInputCss,
		"package.json":       []byte(`{"private":true,"dependencies":{"tailwindcss":"^4"}}`),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), Tailwind.Timeout)
	defer cancel()

	if err := react.runCmd(ctx, dir, "npm", "install", "--silent"); err != nil {
		return fmt.Errorf("npm install: %w", err)
	}

	outputCSS := filepath.Join(dir, "output.css")
	args := append(slices.Clone(Tailwind.Path),
		"-i", filepath.Join(dir, "input.css"),
		"-o", outputCSS,
		"-m",
	)
	if err := react.runCmd(ctx, dir, args[0], args[1:]...); err != nil {
		return fmt.Errorf("tailwind build: %w", err)
	}

	css, err := os.ReadFile(outputCSS)
	if err != nil {
		return fmt.Errorf("read output css: %w", err)
	}
	injectStyleIntoPages(pages, string(css))
	return nil
}

func (react *implReact) writePagesToDir(dir string, pages map[string]*html.Node) error {
	for path, page := range pages {
		var buf bytes.Buffer
		if err := html.Render(&buf, page); err != nil {
			return fmt.Errorf("render %s: %w", path, err)
		}

		pageDir := filepath.Join(dir, "content", path)
		if err := os.MkdirAll(pageDir, 0755); err != nil {
			return err
		}

		if err := os.WriteFile(filepath.Join(pageDir, "index.html"), buf.Bytes(), 0644); err != nil {
			return err
		}
	}
	return nil
}

func (react *implReact) runCmd(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (%s)", err, bytes.TrimSpace(output))
	}
	return nil
}

func (react *implReact) buildDirectoryIndexMdOpt(files map[string][]byte, funcs template.FuncMap, page *bytes.Buffer) (*bytes.Buffer, error) {
	data, ok := files["index.md"]
	if !ok {
		return page, nil
	}

	tmpl, err := template.New("index.md").
		Funcs(funcs).
		Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("error parsing index.md template: %w", err)
	}

	var mdBuf bytes.Buffer
	if err := tmpl.Execute(&mdBuf, nil); err != nil {
		return nil, fmt.Errorf("error executing index.md template: %w", err)
	}

	mdParser := parser.NewWithExtensions(
		parser.CommonExtensions |
			parser.AutoHeadingIDs |
			parser.NoEmptyLineBeforeBlock |
			parser.Tables,
	)
	doc := mdParser.Parse(mdBuf.Bytes())

	htmlRenderer := mdhtml.NewRenderer(mdhtml.RendererOptions{
		Flags: mdhtml.CommonFlags | mdhtml.HrefTargetBlank,
	})

	htmlBytes := markdown.Render(doc, htmlRenderer)
	// todo: find a fix, not a workaround (there's a chance the bug lies in mdhtml lib)
	htmlBytes = bytes.ReplaceAll(htmlBytes, []byte("&amp;lt;"), []byte("&lt;"))
	page.Write([]byte(`<div class="markdown mx-1">`))
	page.Write(htmlBytes)
	page.Write([]byte(`</div>`))

	return page, nil
}

func injectStyleIntoPages(pages map[string]*html.Node, css string) {
	for _, page := range pages {
		head := htmlFindByTag(page, "head")
		if head == nil {
			continue
		}
		style := &html.Node{
			Type: html.ElementNode,
			Data: "style",
		}
		style.AppendChild(&html.Node{
			Type: html.TextNode,
			Data: css,
		})
		head.AppendChild(style)
	}
}

func makeBodyFunc(page *bytes.Buffer) template.FuncMap {
	return template.FuncMap{
		"body": func() template.HTML {
			return template.HTML(page.String())
		},
	}
}

func def[T any](list []T, otherwise T) T {
	if len(list) == 0 {
		return otherwise
	}
	return list[0]
}

type reactYamlLayout struct {
	Title      string                              `yaml:"title"`
	Icon       string                              `yaml:"icon"`
	Meta       map[string]string                   `yaml:"meta,omitempty"`
	Head       []string                            `yaml:"extra,omitempty"`
	Body       []*reactYamlLayoutStringOrList      `yaml:"body,omitempty"`
	Components map[string]reactYamlLayoutComponent `json:"components,omitempty"`
}

type reactYamlLayoutComponent struct {
	Source  string `yaml:"source"`
	Dynamic bool   `yaml:"dynamic"`
}

func (r *reactYamlLayoutComponent) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var source string
	if err := unmarshal(&source); err == nil {
		r.Source = source
		r.Dynamic = false
		return nil
	}

	// If string fails, try to unmarshal into a "shadow" struct to avoid infinite recursion
	type shadow reactYamlLayoutComponent
	var s shadow
	if err := unmarshal(&s); err != nil {
		return err
	}

	r.Source = s.Source
	r.Dynamic = s.Dynamic
	return nil
}

type reactYamlLayoutStringOrList struct {
	String string
	List   []string
}

func (stringOrList *reactYamlLayoutStringOrList) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		stringOrList.String = s
		return nil
	}
	var list []string
	if err := unmarshal(&list); err == nil {
		stringOrList.List = list
		return nil
	}
	return fmt.Errorf("invalid react yaml layout string or list")
}

func htmlFindByTag(node *html.Node, tag string) *html.Node {
	if node.Type == html.ElementNode && node.Data == tag {
		return node
	}
	for child := range node.ChildNodes() {
		if res := htmlFindByTag(child, tag); res != nil {
			return res
		}
	}
	return nil
}

func xhtmlEscapeString(str string) string {
	if strings.Contains(str, "{{") && strings.Contains(str, "}}") {
		return str
	}
	return xhtml.EscapeString(str)
}
func hash(data []byte) string {
	result := sha256.New()
	if _, err := io.Copy(result, bytes.NewReader(data)); err != nil {
		panic(err)
	}
	return hex.EncodeToString(result.Sum(nil))[:16]
}

func templCloneExecute(templ *template.Template, wr io.Writer, data any) error {
	clone, err := templ.Clone()
	if err != nil {
		return err
	}
	return clone.Execute(wr, data)
}
