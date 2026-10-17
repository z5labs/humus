{{- /*
  The Markdown copy of a page, at its URL followed by index.html.md: its
  title as an H1, then its content with the shortcodes rendered, so its links
  to other pages resolve.
*/ -}}
# {{ .Title }}

{{ .RenderShortcodes | strings.TrimSpace }}
