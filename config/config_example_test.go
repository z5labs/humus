// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package config

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"
)

func Example() {
	myIntFromString, _ := Read(
		context.Background(),
		Default(10, Int64FromString(Env("MY_INT"))),
	)

	myIntFromBytes, _ := Read(
		context.Background(),
		Default(10, Int64FromBytes(binary.LittleEndian, ReaderOf(bytes.NewReader([]byte{1, 1, 1, 1, 1, 1, 1, 1})))),
	)

	fmt.Println(myIntFromString)
	fmt.Println(myIntFromBytes)
	// Output:
	// 10
	// 72340172838076673
}

func ExampleUnmarshalJSON() {
	type AppConfig struct {
		Port    int    `json:"port"`
		Env     string `json:"env"`
		Enabled bool   `json:"enabled"`
	}

	appCfgReader := UnmarshalJSON[AppConfig](ReaderOf(strings.NewReader(`{
  "port": 8080,
  "env": "production",
  "enabled": true
}`)))

	appCfg, _ := Read(context.Background(), appCfgReader)

	fmt.Println("port:", appCfg.Port)
	fmt.Println("env:", appCfg.Env)
	fmt.Println("enabled:", appCfg.Enabled)

	// Output:
	// port: 8080
	// env: production
	// enabled: true
}

func ExampleUnmarshalYAML() {
	type AppConfig struct {
		Port    int    `yaml:"port"`
		Env     string `yaml:"env"`
		Enabled bool   `yaml:"enabled"`
	}

	appCfgReader := UnmarshalYAML[AppConfig](ReaderOf(strings.NewReader(`port: 8080
env: production
enabled: true
`)))

	appCfg, _ := Read(context.Background(), appCfgReader)

	fmt.Println("port:", appCfg.Port)
	fmt.Println("env:", appCfg.Env)
	fmt.Println("enabled:", appCfg.Enabled)

	// Output:
	// port: 8080
	// env: production
	// enabled: true
}
