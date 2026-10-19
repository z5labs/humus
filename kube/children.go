// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/baggage"
	"sigs.k8s.io/yaml"
)

// applyPatch is the content type of a server-side apply. Its body is YAML,
// or JSON, which is YAML too.
const applyPatch = "application/apply-patch+yaml"

// carryOn returns r, a request whose scenario has just been stripped, with
// the scenario set on the child it applies, as the package doc's Carrying
// a scenario on to children says. ctx is r's context, and stripped is ctx
// with the scenario stripped from its baggage. point is r's point.
//
// r is changed only when it is a server-side apply of an object, not of a
// subresource, in a JSON or YAML body, made with a reconcile's context, of
// an object whose controller owner reference is the reconciled object. Its
// body then has metadata.annotations[<annotation>] set to the scenario's
// members, and is sent on as JSON. Any other request is sent as it is,
// with its body as it was. r is a clone of the request RoundTrip was given,
// so its fields are changed in place. carryOn closes r's body once it has
// read it, and returns the error of a body it couldn't read.
func carryOn(ctx, stripped context.Context, r *http.Request, point string) (*http.Request, error) {
	rec, ok := reconciledFrom(ctx)
	if !ok || rec.uid == "" || !isObjectApply(r, point) || r.Body == nil || r.Body == http.NoBody {
		return r, nil
	}
	body, err := io.ReadAll(r.Body)
	closeBody(r)
	if err != nil {
		return nil, err
	}
	out, changed := setChildAnnotation(body, rec, scenarioOf(ctx, stripped))
	if !changed {
		out = body
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(out)), nil
	}
	return r, nil
}

// isObjectApply reports whether r, whose point is point, is a server-side
// apply of an object. A point names a subresource after a /, and a path
// that isn't a resource starts with one, and no resource's name has one,
// so the point of an apply of an object is kube:<resource>[.<group>]:patch
// without a /.
func isObjectApply(r *http.Request, point string) bool {
	if r.Method != http.MethodPatch {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != applyPatch {
		return false
	}
	return strings.HasSuffix(point, ":patch") && !strings.Contains(point, "/")
}

// scenarioOf returns the scenario's members of ctx's baggage, which
// stripped's baggage hasn't got, as W3C baggage. The members are sorted,
// so the same scenario is always the same value, and an apply of it
// changes nothing.
func scenarioOf(ctx, stripped context.Context) string {
	rest := baggage.FromContext(stripped)
	var members []string
	for _, m := range baggage.FromContext(ctx).Members() {
		if rest.Member(m.Key()).Key() == "" {
			members = append(members, m.String())
		}
	}
	slices.Sort(members)
	return strings.Join(members, ",")
}

// setChildAnnotation returns body, an apply's JSON or YAML body, as JSON
// with metadata.annotations[rec.annotation] set to value, and true, when
// the object it applies is rec's child: its metadata.ownerReferences has a
// controller reference whose uid is rec's. A body that isn't JSON or YAML
// of an object, or of rec's child, or whose annotations aren't an object,
// returns false.
//
// Numbers are kept as they are written in JSON. YAML is first converted to
// JSON, as the API server converts it.
func setChildAnnotation(body []byte, rec reconciled, value string) ([]byte, bool) {
	if !json.Valid(body) {
		converted, err := yaml.YAMLToJSON(body)
		if err != nil {
			return nil, false
		}
		body = converted
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	metadata, ok := obj["metadata"].(map[string]any)
	if !ok || !controlledBy(metadata, rec) {
		return nil, false
	}
	annotations, ok := metadata["annotations"].(map[string]any)
	switch {
	case ok:
	case metadata["annotations"] == nil:
		annotations = map[string]any{}
		metadata["annotations"] = annotations
	default:
		return nil, false
	}
	annotations[rec.annotation] = value
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// controlledBy reports whether metadata has an owner reference that is a
// controller's, with controller true, and whose uid is rec's, as
// Kubernetes' garbage collector reads a controller reference.
func controlledBy(metadata map[string]any, rec reconciled) bool {
	refs, _ := metadata["ownerReferences"].([]any)
	for _, ref := range refs {
		m, ok := ref.(map[string]any)
		if !ok {
			continue
		}
		controller, _ := m["controller"].(bool)
		uid, _ := m["uid"].(string)
		if controller && uid == string(rec.uid) {
			return true
		}
	}
	return false
}
