{{- /*
  The Markdown copy of a page, at its URL followed by index.html.md: its
  title as an H1, then its content with the shortcodes rendered, so its links
  to other pages resolve, then a link to the next page in the reading order.
*/ -}}
# {{ .Title }}

{{ .RenderShortcodes | strings.TrimSpace }}
{{- with partial "next-page.html" . }}

Next: [{{ .Title }}]({{ .RelPermalink }})
{{- end }}
