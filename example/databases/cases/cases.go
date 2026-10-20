// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the databases controller's black-box cases, run against a cluster it reconciles.
package cases

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/z5labs/humus/fault/jsonscenario"
)

const (
	retainPolicy = "Retain"
	deletePolicy = "Delete"
	poll         = 250 * time.Millisecond
)

const (
	finalizer = "postgres.example.com/finalizer"
	slowBy    = 2 * time.Second
	held      = 3 * time.Second
)

var databases = schema.GroupVersionResource{Group: "postgres.example.com", Version: "v1alpha1", Resource: "databases"}

type Target struct {
	Config      *rest.Config
	Prefix      string
	Annotation  string
	ScenarioKey string
	Postgres    string
	Timeout     time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "Provisioning", Run: Provisioning},
	{Name: "Rotation", Run: Rotation},
	{Name: "Deletion", Run: Deletion},
	{Name: "Rules", Run: Rules},
	{Name: "PostgreSQLDown", Run: PostgreSQLDown},
	{Name: "SecretApplyFailing", Run: SecretApplyFailing},
	{Name: "SlowPostgreSQL", Run: SlowPostgreSQL},
	{Name: "DropFailing", Run: DropFailing},
}

func Provisioning(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)
	name := databaseName()

	c.create(ctx, t, "orders", name, deletePolicy)
	c.awaitReady(ctx, t, "orders")

	creds := c.secret(ctx, t, "orders")
	require.Equal(t, name, creds.database)
	require.Equal(t, name, creds.username)
	requireLogsIn(ctx, t, target, creds)
}

func Rotation(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)

	c.create(ctx, t, "orders", databaseName(), deletePolicy)
	c.awaitReady(ctx, t, "orders")
	old := c.secret(ctx, t, "orders")
	requireLogsIn(ctx, t, target, old)

	err := c.kube.CoreV1().Secrets(c.namespace).Delete(ctx, old.name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &old.uid},
	})
	require.NoError(t, err)

	rotated := c.awaitSecret(ctx, t, "orders", func(s credentials) bool { return s.uid != old.uid })
	require.NotEqual(t, old.password, rotated.password)
	requireLogsIn(ctx, t, target, rotated)
	requireRefused(ctx, t, target, old)
}

func Deletion(t testing.TB, target Target) {
	ctx := bounded(t, target)
	dropped, retained := databaseName(), databaseName()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
		defer cancel()
		dropRetained(ctx, t, target, retained)
	})
	c := newCluster(ctx, t, target)

	c.create(ctx, t, "dropped", dropped, deletePolicy)
	c.create(ctx, t, "retained", retained, retainPolicy)
	c.awaitReady(ctx, t, "dropped")
	c.awaitReady(ctx, t, "retained")

	for _, db := range []string{"dropped", "retained"} {
		err := c.dynamic.Resource(databases).Namespace(c.namespace).Delete(ctx, db, metav1.DeleteOptions{})
		require.NoError(t, err)
	}
	for _, db := range []string{"dropped", "retained"} {
		c.awaitGone(ctx, t, db)
	}

	role, database := exists(ctx, t, target, dropped)
	require.False(t, role, "the role of a Database with the Delete policy")
	require.False(t, database, "the database of a Database with the Delete policy")
	role, database = exists(ctx, t, target, retained)
	require.True(t, role, "the role of a Database with the Retain policy")
	require.True(t, database, "the database of a Database with the Retain policy")
}

func Rules(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)
	dbs := c.dynamic.Resource(databases).Namespace(c.namespace)

	_, err := dbs.Create(ctx, database("invalid", "Orders-DB", deletePolicy), metav1.CreateOptions{})
	require.True(t, apierrors.IsInvalid(err), "an invalid name: %v", err)
	require.ErrorContains(t, err, "spec.name")

	c.create(ctx, t, "orders", databaseName(), deletePolicy)
	_, err = dbs.Patch(ctx, "orders", types.MergePatchType, []byte(`{"spec":{"name":"`+databaseName()+`"}}`), metav1.PatchOptions{})
	require.True(t, apierrors.IsInvalid(err), "a changed name: %v", err)
	require.ErrorContains(t, err, "name is immutable")
}

func PostgreSQLDown(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)

	c.rehearse(ctx, t, "orders", databaseName(), deletePolicy, abort("postgresql:*"))
	c.awaitCondition(ctx, t, "orders", metav1.ConditionFalse, "PostgresUnavailable")

	c.unannotate(ctx, t, "orders")
	c.awaitReady(ctx, t, "orders")
	requireLogsIn(ctx, t, target, c.secret(ctx, t, "orders"))
}

func SecretApplyFailing(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)

	c.rehearse(ctx, t, "orders", databaseName(), deletePolicy, abort("kube:secrets:patch"))
	c.awaitCondition(ctx, t, "orders", metav1.ConditionFalse, "SecretApplyFailed")

	c.unannotate(ctx, t, "orders")
	c.awaitReady(ctx, t, "orders")
	requireLogsIn(ctx, t, target, c.secret(ctx, t, "orders"))
}

func SlowPostgreSQL(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)

	c.rehearse(ctx, t, "orders", databaseName(), deletePolicy, delay("postgresql:*", slowBy))
	c.awaitReady(ctx, t, "orders")
	requireLogsIn(ctx, t, target, c.secret(ctx, t, "orders"))
}

func DropFailing(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)
	name := databaseName()

	c.rehearse(ctx, t, "orders", name, deletePolicy, abort("postgresql:drop"))
	c.awaitReady(ctx, t, "orders")
	err := c.dynamic.Resource(databases).Namespace(c.namespace).Delete(ctx, "orders", metav1.DeleteOptions{})
	require.NoError(t, err)

	c.holds(ctx, t, "orders")
	role, database := exists(ctx, t, target, name)
	require.True(t, role, "the role of a Database whose drop fails")
	require.True(t, database, "the database of a Database whose drop fails")

	c.unannotate(ctx, t, "orders")
	c.awaitGone(ctx, t, "orders")
	role, database = exists(ctx, t, target, name)
	require.False(t, role, "the role of a Database whose drop recovered")
	require.False(t, database, "the database of a Database whose drop recovered")
}

func bounded(t testing.TB, target Target) context.Context {
	require.Positive(t, target.Timeout, "Target.Timeout")
	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout)
	t.Cleanup(cancel)
	return ctx
}

func databaseName() string {
	return "db_" + strings.ToLower(rand.Text())
}

func database(name, dbName, policy string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": databases.GroupVersion().String(),
		"kind":       "Database",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"name": dbName, "deletionPolicy": policy},
	}}
}

type cluster struct {
	target    Target
	kube      kubernetes.Interface
	dynamic   dynamic.Interface
	namespace string
}

func newCluster(ctx context.Context, t testing.TB, target Target) cluster {
	t.Helper()
	require.NotNil(t, target.Config, "Target.Config")
	require.NotEmpty(t, target.Prefix, "Target.Prefix")
	kc, err := kubernetes.NewForConfig(target.Config)
	require.NoError(t, err)
	dc, err := dynamic.NewForConfig(target.Config)
	require.NoError(t, err)

	ns, err := kc.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: target.Prefix},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
		defer cancel()
		deleteNamespace(ctx, t, kc, ns.Name)
	})
	return cluster{target: target, kube: kc, dynamic: dc, namespace: ns.Name}
}

func deleteNamespace(ctx context.Context, t testing.TB, kc kubernetes.Interface, name string) {
	t.Helper()
	err := kc.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	require.NoError(t, err, "deleting the namespace %s", name)
	await(ctx, t, "the namespace "+name+" to be gone", func(ctx context.Context) (bool, error) {
		_, err := kc.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

func (c cluster) create(ctx context.Context, t testing.TB, name, dbName, policy string) {
	t.Helper()
	_, err := c.dynamic.Resource(databases).Namespace(c.namespace).Create(ctx, database(name, dbName, policy), metav1.CreateOptions{})
	require.NoError(t, err, "creating the Database %s/%s", c.namespace, name)
}

func (c cluster) awaitReady(ctx context.Context, t testing.TB, name string) {
	t.Helper()
	c.awaitCondition(ctx, t, name, metav1.ConditionTrue, "")
}

func (c cluster) awaitCondition(ctx context.Context, t testing.TB, name string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	what := "the Database " + c.namespace + "/" + name + " to be Ready " + string(status)
	if reason != "" {
		what += " with " + reason
	}
	await(ctx, t, what, func(ctx context.Context) (bool, error) {
		db, err := c.dynamic.Resource(databases).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		conditions, _, err := unstructured.NestedSlice(db.Object, "status", "conditions")
		if err != nil {
			return false, err
		}
		for _, cond := range conditions {
			m, ok := cond.(map[string]any)
			if ok && m["type"] == "Ready" {
				return m["status"] == string(status) && (reason == "" || m["reason"] == reason), nil
			}
		}
		return false, nil
	})
}

func (c cluster) rehearse(ctx context.Context, t testing.TB, name, dbName, policy string, rules ...fit.Rule) {
	t.Helper()
	require.NotEmpty(t, c.target.Annotation, "Target.Annotation")
	require.NotEmpty(t, c.target.ScenarioKey, "Target.ScenarioKey")
	codec, err := jsonscenario.New(c.target.ScenarioKey)
	require.NoError(t, err, "Target.ScenarioKey")
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)

	db := database(name, dbName, policy)
	db.SetAnnotations(map[string]string{c.target.Annotation: b.String()})
	_, err = c.dynamic.Resource(databases).Namespace(c.namespace).Create(ctx, db, metav1.CreateOptions{})
	require.NoError(t, err, "creating the Database %s/%s", c.namespace, name)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.target.Timeout)
		defer cancel()
		c.unannotate(ctx, t, name)
	})
}

func (c cluster) unannotate(ctx context.Context, t testing.TB, name string) {
	t.Helper()
	patch := `{"metadata":{"annotations":{"` + c.target.Annotation + `":null}}}`
	_, err := c.dynamic.Resource(databases).Namespace(c.namespace).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	require.NoError(t, err, "removing the scenario from the Database %s/%s", c.namespace, name)
}

func (c cluster) holds(ctx context.Context, t testing.TB, name string) {
	t.Helper()
	until := time.After(held)
	for {
		db, err := c.dynamic.Resource(databases).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err, "the Database %s/%s whose drop fails", c.namespace, name)
		require.NotNil(t, db.GetDeletionTimestamp(), "the Database %s/%s isn't being deleted", c.namespace, name)
		require.Contains(t, db.GetFinalizers(), finalizer, "the finalizers of the Database %s/%s", c.namespace, name)
		select {
		case <-ctx.Done():
			require.FailNowf(t, "timed out", "holding the Database %s/%s: %v", c.namespace, name, context.Cause(ctx))
		case <-until:
			return
		case <-time.After(poll):
		}
	}
}

func abort(point string) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Abort: true}}
}

func delay(point string, d time.Duration) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Delay: d}}
}

func (c cluster) awaitGone(ctx context.Context, t testing.TB, name string) {
	t.Helper()
	await(ctx, t, "the Database "+c.namespace+"/"+name+" to be gone", func(ctx context.Context) (bool, error) {
		_, err := c.dynamic.Resource(databases).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

type credentials struct {
	name     string
	uid      types.UID
	database string
	username string
	password string
}

func (c cluster) secretName(ctx context.Context, t testing.TB, name string) string {
	t.Helper()
	db, err := c.dynamic.Resource(databases).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	secretName, _, err := unstructured.NestedString(db.Object, "status", "secretName")
	require.NoError(t, err)
	require.NotEmpty(t, secretName, "the Database %s/%s's status.secretName", c.namespace, name)
	return secretName
}

func (c cluster) secret(ctx context.Context, t testing.TB, name string) credentials {
	t.Helper()
	return c.awaitSecret(ctx, t, name, func(credentials) bool { return true })
}

func (c cluster) awaitSecret(ctx context.Context, t testing.TB, name string, want func(credentials) bool) credentials {
	t.Helper()
	secretName := c.secretName(ctx, t, name)
	var found credentials
	await(ctx, t, "the Secret "+c.namespace+"/"+secretName, func(ctx context.Context) (bool, error) {
		s, err := c.kube.CoreV1().Secrets(c.namespace).Get(ctx, secretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		found = credentials{
			name:     s.Name,
			uid:      s.UID,
			database: string(s.Data["database"]),
			username: string(s.Data["username"]),
			password: string(s.Data["password"]),
		}
		return found.password != "" && want(found), nil
	})
	return found
}

func await(ctx context.Context, t testing.TB, what string, done func(ctx context.Context) (bool, error)) {
	t.Helper()
	var last error
	for {
		ok, err := done(ctx)
		if ok {
			return
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			require.FailNowf(t, "timed out", "waiting for %s: %v (last error: %v)", what, context.Cause(ctx), last)
		case <-time.After(poll):
		}
	}
}

func connect(ctx context.Context, target Target, creds credentials) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(target.Postgres)
	if err != nil {
		return nil, err
	}
	cfg.User = creds.username
	cfg.Password = creds.password
	cfg.Database = creds.database
	return pgx.ConnectConfig(ctx, cfg)
}

func requireLogsIn(ctx context.Context, t testing.TB, target Target, creds credentials) {
	t.Helper()
	conn, err := connect(ctx, target, creds)
	require.NoError(t, err, "connecting with the Secret's credentials")
	defer func() { _ = conn.Close(context.Background()) }()

	var user, owner string
	err = conn.QueryRow(ctx,
		`SELECT current_user, pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database()`,
	).Scan(&user, &owner)
	require.NoError(t, err)
	require.Equal(t, creds.username, user)
	require.Equal(t, creds.username, owner, "the owner of the database %s", creds.database)
}

func requireRefused(ctx context.Context, t testing.TB, target Target, creds credentials) {
	t.Helper()
	conn, err := connect(ctx, target, creds)
	if conn != nil {
		_ = conn.Close(context.Background())
	}
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "connecting with the old password")
	require.Equal(t, "28P01", pgErr.Code, "connecting with the old password")
}

func admin(ctx context.Context, t testing.TB, target Target) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, target.Postgres)
	require.NoError(t, err, "connecting with Target.Postgres")
	return conn
}

func exists(ctx context.Context, t testing.TB, target Target, name string) (role, database bool) {
	t.Helper()
	conn := admin(ctx, t, target)
	defer func() { _ = conn.Close(context.Background()) }()
	err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1), EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`,
		name,
	).Scan(&role, &database)
	require.NoError(t, err)
	return role, database
}

func dropRetained(ctx context.Context, t testing.TB, target Target, name string) {
	t.Helper()
	conn := admin(ctx, t, target)
	defer func() { _ = conn.Close(context.Background()) }()
	ident := pgx.Identifier{name}.Sanitize()
	_, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "DROP ROLE IF EXISTS "+ident)
	require.NoError(t, err)
}
