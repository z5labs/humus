# The Database resource

The `api/v1alpha1` package declares the resource as Go values. `kube`
reflects its CRD from them.

## The types

The spec names the database, and says what deleting the `Database` does to
it. The status names the Secret, and holds the conditions:

```go
type DeletionPolicy string

const (
	Retain DeletionPolicy = "Retain"
	Delete DeletionPolicy = "Delete"
)

type DatabaseSpec struct {
	Name           string         `json:"name"`
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

type DatabaseStatus struct {
	SecretName string             `json:"secretName,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

## The resource and its rules

`Database` is the `kube.Resource`, with a rule for each field:

```go
var (
	spec   DatabaseSpec
	status DatabaseStatus

	Database = kube.Resource[DatabaseSpec, DatabaseStatus]{
		Group:   "postgres.example.com",
		Version: "v1alpha1",
		Kind:    "Database",
		Plural:  "databases",
		Scope:   kube.Namespaced,
		Rules: []kube.TypeRules{
			kube.Rules(&spec,
				kube.Field(&spec.Name).
					Description("The name of the database in the server, and of the role that owns it.").
					Pattern(regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)).
					Validation("self == oldSelf", "name is immutable"),
				kube.Field(&spec.DeletionPolicy).
					Description("What deleting the Database does: Retain keeps the database and its role, and Delete drops both.").
					Enum(Retain, Delete).
					Default(Retain),
			),
			kube.Rules(&status,
				kube.Field(&status.SecretName).Description("The Secret holding the database's credentials."),
				kube.Field(&status.Conditions).ListMap("type"),
			),
		},
	}
)
```

- `name` is an unquoted PostgreSQL identifier, of at most 63 bytes.
- The CEL rule `self == oldSelf` makes `name` immutable, since a renamed
  `Database` would leave its old database behind.
- `deletionPolicy` defaults to `Retain`, so a `Database` deleted by mistake
  loses no data.
- `conditions` is a list map keyed by `type`, so a server-side apply
  merges the conditions by their type.

## The golden CRD

`deploy/crd.yaml` is the CRD, reflected from `Database`. A test fails when
the two differ, and rewrites the file with `-update`:

```go
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
```

```sh
go test ./api/v1alpha1 -update
```

The spec's schema, in the CRD:

```yaml
spec:
  properties:
    deletionPolicy:
      default: Retain
      description: 'What deleting the Database does: Retain keeps the database
        and its role, and Delete drops both.'
      enum:
      - Retain
      - Delete
      type: string
    name:
      description: The name of the database in the server, and of the role
        that owns it.
      pattern: ^[a-z_][a-z0-9_]{0,62}$
      type: string
      x-kubernetes-validations:
      - message: name is immutable
        rule: self == oldSelf
  required:
  - name
  type: object
```

The kustomization applies the CRD with the rest of the controller.
