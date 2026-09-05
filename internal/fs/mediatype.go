package fs

import (
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// byExtension pins the media types the surface cares about. Go's mime table
// disagrees with IANA on some of these and varies by host, so they are fixed
// here.
var byExtension = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".log":      "text/plain",
	".yaml":     "application/yaml",
	".yml":      "application/yaml",
	".json":     "application/json",
	".toml":     "application/toml",
	".csv":      "text/csv",
	".html":     "text/html",
	".css":      "text/css",
	".js":       "text/javascript",
	".ts":       "text/typescript",
	".go":       "text/x-go",
	".py":       "text/x-python",
	".rs":       "text/x-rust",
	".c":        "text/x-c",
	".h":        "text/x-c",
	".sh":       "text/x-shellscript",
	".sql":      "application/sql",
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".gif":      "image/gif",
	".svg":      "image/svg+xml",
	".pdf":      "application/pdf",
}

// MediaType guesses the IANA media type of a file from its extension, falling
// back to sniffing the first bytes of its content.
func MediaType(path string, sample []byte) string {
	ext := strings.ToLower(filepath.Ext(path))
	if mt, ok := byExtension[ext]; ok {
		return mt
	}
	if mt := mime.TypeByExtension(ext); mt != "" {
		return strings.TrimSpace(strings.SplitN(mt, ";", 2)[0])
	}
	if len(sample) == 0 {
		return "application/octet-stream"
	}
	sniffed := strings.TrimSpace(strings.SplitN(http.DetectContentType(sample), ";", 2)[0])
	if sniffed == "text/plain" && !utf8.Valid(sample) {
		return "application/octet-stream"
	}
	return sniffed
}

// IsText reports whether a media type is returned as an MCP text block.
func IsText(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	if strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+yaml") || strings.HasSuffix(mediaType, "+xml") {
		return true
	}
	switch mediaType {
	case "application/json", "application/yaml", "application/x-yaml", "application/toml",
		"application/xml", "application/sql", "application/javascript", "application/x-sh":
		return true
	}
	return false
}

// IsImage reports whether a media type is returned as an MCP image block.
func IsImage(mediaType string) bool {
	return mediaType == "image/png" || mediaType == "image/jpeg"
}
