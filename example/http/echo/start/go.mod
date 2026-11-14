module github.com/z5labs/humus/example/http/echo/start

go 1.27.0

replace github.com/z5labs/humus/example/http/echo/cases => ../cases

require (
	github.com/go-playground/validator/v10 v10.30.6
	github.com/labstack/echo/v5 v5.4.5
	github.com/stretchr/testify v1.12.1
	github.com/z5labs/humus/example/http/echo/cases v0.0.0-00010101000000-000000000000
)

require (
	github.com/gabriel-vasile/mimetype v1.4.16 // indirect
	github.com/go-playground/locales v0.14.2 // indirect
	github.com/go-playground/universal-translator v0.18.2 // indirect
	github.com/leodido/go-urn v1.5.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.59.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.44.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
