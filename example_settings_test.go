// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
)

// The configuration page of the docs site quotes the code in this file, and
// TestDocs_code_samples checks that every quote still matches it.
//
// Settings, DefaultSettings, the Check functions and NewGreeter stand in for
// those of a humus package with settings, in the shape every such package
// gives them, as the telemetry package does. example_telemetry_test.go
// sources telemetry.Settings with the same pattern.

// Settings configures a Greeter. Every field is a plain value, and a field's
// zero value means zero, never "use the default".
type Settings struct {
	// Greeting is what the Greeter says. Default "hello".
	Greeting string

	// ReadTimeout bounds reading a whole request. Default 5s. Zero means
	// no timeout. Must not be negative; checked by CheckReadTimeout.
	ReadTimeout time.Duration

	// MaxConns is how many connections the Greeter serves at once.
	// Default 100. Must be positive; checked by CheckMaxConns.
	MaxConns int
}

// DefaultSettings returns the defaults of a Greeter.
func DefaultSettings() Settings {
	return Settings{
		Greeting:    "hello",
		ReadTimeout: 5 * time.Second,
		MaxConns:    100,
	}
}

// CheckReadTimeout reports why d is not a valid ReadTimeout, or nil if it
// is.
func CheckReadTimeout(d time.Duration) error {
	if d < 0 {
		return errors.New("must not be negative")
	}
	return nil
}

// CheckMaxConns reports why n is not a valid MaxConns, or nil if it is.
func CheckMaxConns(n int) error {
	if n <= 0 {
		return errors.New("must be positive")
	}
	return nil
}

// Greeter is the Service of the App in Example_settings. It writes its
// settings and returns, as a job does.
type Greeter struct {
	settings Settings
	out      io.Writer
}

// NewGreeter returns a Greeter configured by s that writes to out. It runs
// every check, and its error names each bad field, so a Settings built in
// code, without config, is checked too.
func NewGreeter(s Settings, out io.Writer) (*Greeter, error) {
	var errs []error
	if err := CheckReadTimeout(s.ReadTimeout); err != nil {
		errs = append(errs, fmt.Errorf("ReadTimeout: %w", err))
	}
	if err := CheckMaxConns(s.MaxConns); err != nil {
		errs = append(errs, fmt.Errorf("MaxConns: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Greeter{settings: s, out: out}, nil
}

func (g *Greeter) Serve(ctx context.Context) error {
	_, err := fmt.Fprintf(g.out, "%s: read timeout %s, up to %d connections\n",
		g.settings.Greeting, g.settings.ReadTimeout, g.settings.MaxConns)
	return err
}

func (g *Greeter) Shutdown(ctx context.Context) error {
	return nil
}

// GreeterInit is the Init of the App in Example_settings. It sources the
// Greeter's Settings from the environment with names and formats of its
// own choosing.
func GreeterInit(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) Settings {
		return Settings{
			// The file is read as part of the setting, so a missing file
			// is a config error named after the variable.
			Greeting:    r.Get(env("GREETING_FILE").Parse(readFile(s.Host.FS)).Default(d.Greeting)),
			ReadTimeout: r.Get(env("READ_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadTimeout).Validate(CheckReadTimeout)),
			MaxConns:    r.Get(env("MAX_CONNS").Parse(strconv.Atoi).Default(d.MaxConns).Validate(CheckMaxConns)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78: every bad variable, one line each
	}

	g, err := NewGreeter(settings, s.Process.Stdout)
	if err != nil {
		return nil, err
	}
	return []humus.Service{g}, nil
}

// readFile returns a parser for config.Value.Parse that reads the whole
// file a variable names, through fsys.
func readFile(fsys humus.FS) func(name string) (string, error) {
	return func(name string) (string, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(f)
		return string(b), err
	}
}

// runGreeter runs the App of Example_settings on a bedrocktest Process with
// env and a humustest Host holding files, and prints what the App wrote, the
// errors it logged and its exit code.
func runGreeter(env map[string]string, files map[string][]byte) {
	t := new(exampleTB)
	defer t.runCleanups()

	h := humustest.NewHost(t, humustest.WithFiles(files))
	tel := humustest.NewTelemetry()
	app := humus.App{
		Host:                h.Host(),
		ShutdownGracePeriod: 10 * time.Second,
		Telemetry:           tel.Builder(),
		Init:                GreeterInit,
	}
	fp := bedrocktest.NewProcess(t, bedrocktest.WithEnv(env))
	status := app.Run(context.Background(), fp.Process())

	fmt.Print(fp.Stdout())
	for _, msg := range errorLogs(tel) {
		fmt.Println(msg)
	}
	fmt.Println("exit code:", status.Code())
}

// An App's Init sources the Settings of the packages it uses, with config.
// Each value starts from the package's default and is checked after it, so
// a malformed or out-of-range value, set or defaulted, exits 78 with an
// error naming the variable. Every bad variable is reported at once.
func Example_settings() {
	// One malformed variable and one out of range, named together.
	runGreeter(map[string]string{"READ_TIMEOUT": "-5s", "MAX_CONNS": "lots"}, nil)

	// A file that does not exist, named after the variable that names it.
	// A relative name is relative to the humustest Host's directory, whose
	// FS opens files through an os.Root, hence openat rather than open.
	runGreeter(map[string]string{"GREETING_FILE": "etc/greeter/greeting.txt"}, nil)

	// The same file, there this time, read through the Host's FS.
	runGreeter(
		map[string]string{"GREETING_FILE": "etc/greeter/greeting.txt"},
		map[string][]byte{"etc/greeter/greeting.txt": []byte("hi")},
	)

	// Nothing set: every field takes its default.
	runGreeter(nil, nil)
	// Output:
	// init: config READ_TIMEOUT: must not be negative
	// config MAX_CONNS: strconv.Atoi: parsing "lots": invalid syntax
	// exit code: 78
	// init: config GREETING_FILE: openat etc/greeter/greeting.txt: no such file or directory
	// exit code: 78
	// hi: read timeout 5s, up to 100 connections
	// exit code: 0
	// hello: read timeout 5s, up to 100 connections
	// exit code: 0
}

// A Settings built in code never passes through config, so the package's
// constructor checks it, and its error names each bad field. A zero field
// is zero, not the default, so it is checked like any other value.
func Example_settingsInCode() {
	settings := DefaultSettings()
	settings.ReadTimeout = -time.Second
	settings.MaxConns = 0

	_, err := NewGreeter(settings, io.Discard)
	fmt.Println(err)
	// Output:
	// ReadTimeout: must not be negative
	// MaxConns: must be positive
}
