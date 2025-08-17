// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"net/http"
	"regexp"

	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/sdk-go/ptr"
)

type parameter struct {
	def        *openapi3.Parameter
	validators []func(*http.Request) error
}

// ParameterOption
type ParameterOption func(*parameter)

// Required
func Required() ParameterOption {
	return func(p *parameter) {
		p.def.Required = ptr.Ref(true)

		p.validators = append(p.validators, func(r *http.Request) error {
			// TODO
			return nil
		})
	}
}

// Regex
func Regex(re *regexp.Regexp) ParameterOption {
	return func(p *parameter) {
		if p.def.Schema == nil {
			p.def.Schema = &openapi3.SchemaOrRef{
				Schema: &openapi3.Schema{},
			}
		}

		p.def.Schema.Schema.Pattern = ptr.Ref(re.String())

		p.validators = append(p.validators, func(r *http.Request) error {
			// TODO
			return nil
		})
	}
}

// OneOf
func OneOf(vals ...string) ParameterOption {
	return func(p *parameter) {
		if p.def.Schema == nil {
			p.def.Schema = &openapi3.SchemaOrRef{
				Schema: &openapi3.Schema{},
			}
		}

		p.def.Schema.Schema.Enum = nil

		p.validators = append(p.validators, func(r *http.Request) error {
			return nil
		})
	}
}

// ValidateParamWith
func ValidateParamWith(f func(*http.Request) error) ParameterOption {
	return func(p *parameter) {
		p.validators = append(p.validators, f)
	}
}
