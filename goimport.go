package main

import (
	"fmt"
	"html"
	"net/http"
	"strings"
)

// projectFromPath extracts the first path segment as the package/project name.
// "/cliflags" and "/cliflags/..." both yield "cliflags". Empty or "/" yield "".
func projectFromPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return ""
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

// repoURLFor builds the self-hosted git clone URL for a project.
// Template placeholders: {domain} (substituted with goImportDomain), {repo}.
func repoURLFor(cfg *serverConfig, project string) string {
	url := cfg.goImportGitURL
	url = strings.ReplaceAll(url, "{domain}", cfg.goImportDomain)
	url = strings.ReplaceAll(url, "{repo}", project)
	return url
}

// goImportMeta returns the content value for the go-import meta tag
// (hosting decision A: self-hosted git server on the same domain).
func goImportMeta(cfg *serverConfig, project string) string {
	importPath := cfg.goImportDomain + "/" + project
	repoURL := repoURLFor(cfg, project)
	return fmt.Sprintf("%s git %s", importPath, repoURL)
}

func serveGoImport(w http.ResponseWriter, r *http.Request, cfg *serverConfig) {
	project := projectFromPath(r.URL.Path)
	if project == "" {
		http.NotFound(w, r)
		return
	}

	meta := goImportMeta(cfg, project)
	escMeta := html.EscapeString(meta)
	escDomain := html.EscapeString(cfg.goImportDomain)
	escProject := html.EscapeString(project)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
<meta name="go-import" content="%s">
<title>%s/%s</title>
</head>
<body>
<code>%s</code>
</body>
</html>
`, escMeta, escDomain, escProject, escMeta)
}
