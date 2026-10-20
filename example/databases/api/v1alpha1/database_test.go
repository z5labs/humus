// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package v1alpha1_test

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/z5labs/humus/example/databases/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden CRD in deploy")

func TestDatabaseCRD(t *testing.T) {
	crd, err := v1alpha1.Database.CRD()
	require.NoError(t, err)
	got, err := yaml.Marshal(crd)
	require.NoError(t, err)

	const path = "../../deploy/crd.yaml"
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
}

func schema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	crd, err := v1alpha1.Database.CRD()
	require.NoError(t, err)
	require.Equal(t, "databases.postgres.example.com", crd.Name)
	require.Equal(t, apiextensionsv1.NamespaceScoped, crd.Spec.Scope)
	require.Len(t, crd.Spec.Versions, 1)
	require.Equal(t, "v1alpha1", crd.Spec.Versions[0].Name)
	return *crd.Spec.Versions[0].Schema.OpenAPIV3Schema
}

func TestDatabase_spec_name_is_required(t *testing.T) {
	t.Parallel()

	spec := schema(t).Properties["spec"]
	require.Equal(t, []string{"name"}, spec.Required)
}

func TestDatabase_spec_name_is_a_postgres_identifier(t *testing.T) {
	t.Parallel()

	name := schema(t).Properties["spec"].Properties["name"]
	require.Equal(t, "string", name.Type)
	require.Equal(t, `^[a-z_][a-z0-9_]{0,62}$`, name.Pattern)
}

func TestDatabase_spec_name_is_immutable(t *testing.T) {
	t.Parallel()

	name := schema(t).Properties["spec"].Properties["name"]
	require.Equal(t, apiextensionsv1.ValidationRules{
		{Rule: "self == oldSelf", Message: "name is immutable"},
	}, name.XValidations)
}

func TestDatabase_spec_deletionPolicy_is_Retain_or_Delete_and_defaults_to_Retain(t *testing.T) {
	t.Parallel()

	policy := schema(t).Properties["spec"].Properties["deletionPolicy"]
	require.Equal(t, "string", policy.Type)
	require.Equal(t, []apiextensionsv1.JSON{{Raw: []byte(`"Retain"`)}, {Raw: []byte(`"Delete"`)}}, policy.Enum)
	require.NotNil(t, policy.Default)
	require.JSONEq(t, `"Retain"`, string(policy.Default.Raw))
}

func TestDatabase_status_conditions_is_a_list_map_keyed_by_type(t *testing.T) {
	t.Parallel()

	status := schema(t).Properties["status"]
	conditions := status.Properties["conditions"]
	require.Equal(t, "array", conditions.Type)
	require.NotNil(t, conditions.XListType)
	require.Equal(t, "map", *conditions.XListType)
	require.Equal(t, []string{"type"}, conditions.XListMapKeys)
	require.Contains(t, conditions.Items.Schema.Required, "type")
	require.Equal(t, "string", status.Properties["secretName"].Type)
}
