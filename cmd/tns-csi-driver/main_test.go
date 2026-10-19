package main

import (
	"flag"
	"testing"
)

func TestResolveAPIKey(t *testing.T) {
	env := func(v string) func(string) string {
		return func(name string) string {
			if name == truenasKeyEnvName {
				return v
			}
			return ""
		}
	}
	tests := []struct {
		name    string
		flagVal string
		envVal  string
		want    string
	}{
		{name: "env only (chart default: keeps the key off the command line)", envVal: "from-env", want: "from-env"},
		{name: "flag wins for backward compatibility", flagVal: "from-flag", envVal: "from-env", want: "from-flag"},
		{name: "flag only", flagVal: "from-flag", want: "from-flag"},
		{name: "neither", want: ""},
		{name: "whitespace from a secret is trimmed", envVal: "  key\n", want: "key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveAPIKey(tt.flagVal, env(tt.envVal)); got != tt.want {
				t.Errorf("resolveAPIKey(%q, env=%q) = %q, want %q", tt.flagVal, tt.envVal, got, tt.want)
			}
		})
	}
}

func TestResolveNVMeNodeProfile(t *testing.T) {
	getenv := func(v string) func(string) string {
		return func(name string) string {
			if name == nvmeNodeProfileEnvName {
				return v
			}
			return ""
		}
	}
	tests := []struct {
		name   string
		envVal string
		want   string
		args   []string
	}{
		{name: "neither", want: ""},
		{name: "env only", envVal: "/fixture/env.json", want: "/fixture/env.json"},
		{name: "flag only", args: []string{"--nvmeof-node-profile=/fixture/flag.json"}, want: "/fixture/flag.json"},
		{name: "flag overrides env", args: []string{"--nvmeof-node-profile=/fixture/flag.json"}, envVal: "/fixture/env.json", want: "/fixture/flag.json"},
		{name: "explicit empty flag disables env", args: []string{"--nvmeof-node-profile="}, envVal: "/fixture/env.json", want: ""},
		{name: "unrelated flag keeps env", args: []string{"--unrelated"}, envVal: "/fixture/env.json", want: "/fixture/env.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags := flag.NewFlagSet("fixture", flag.ContinueOnError)
			flags.String(nvmeNodeProfileFlagName, "", "")
			flags.Bool("unrelated", false, "")
			if err := flags.Parse(tt.args); err != nil {
				t.Fatalf("parse %q: %v", tt.args, err)
			}
			if got := resolveNVMeNodeProfile(flags, getenv(tt.envVal)); got != tt.want {
				t.Errorf("resolveNVMeNodeProfile(%q, env=%q) = %q, want %q", tt.args, tt.envVal, got, tt.want)
			}
		})
	}
}
