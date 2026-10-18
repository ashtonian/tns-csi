package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestHelmAuthenticationRendering(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm is required for rendering checks")
	}
	for _, tc := range []struct{ name, username, external string }{
		{"legacy managed", "", ""},
		{"modern managed", "fixture-user", ""},
		{"legacy external", "", "external-credentials"},
		{"modern external owns username", "ignored-chart-user", "external-credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			args := []string{"template", "auth-fixture", filepath.Join("..", "..", "charts", "tns-csi-driver"),
				"--set", "truenas.url=wss://fixture.example/api/current", "--set", "truenas.apiKey=fixture-key",
				"--set", "storageClasses[0].server=fixture.example", "--set-string", "truenas.username=" + tc.username,
				"--set-string", "truenas.existingSecret=" + tc.external}
			output, err := exec.CommandContext(ctx, "helm", args...).Output()
			if err != nil {
				t.Fatalf("Helm template failed: %v", err)
			}
			decoder := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
			workloads, secrets := 0, 0
			for {
				var doc struct {
					Kind       string            `json:"kind"`
					StringData map[string]string `json:"stringData"`
					Spec       struct {
						Template corev1.PodTemplateSpec `json:"template"`
					} `json:"spec"`
				}
				if err := decoder.Decode(&doc); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if doc.Kind == "Secret" {
					secrets++
					if doc.StringData["username"] != tc.username {
						t.Error("managed Secret username does not match")
					}
					if tc.username == "" {
						if _, ok := doc.StringData["username"]; ok {
							t.Error("empty username key must be omitted")
						}
					}
				}
				if doc.Kind != "Deployment" && doc.Kind != "DaemonSet" {
					continue
				}
				for _, container := range doc.Spec.Template.Spec.Containers {
					var username, key *corev1.SecretKeySelector
					for _, env := range container.Env {
						if env.ValueFrom == nil {
							continue
						}
						if env.Name == "TNS_USERNAME" {
							username = env.ValueFrom.SecretKeyRef
						}
						if env.Name == "TNS_API_KEY" {
							key = env.ValueFrom.SecretKeyRef
						}
					}
					if key == nil {
						continue
					}
					workloads++
					if username == nil || username.Key != "username" || username.Optional == nil || !*username.Optional || username.Name != key.Name {
						t.Fatal("username Secret reference must be optional and share the credential Secret")
					}
					if tc.external != "" && username.Name != tc.external {
						t.Error("chart credentials override external Secret")
					}
					for _, arg := range container.Args {
						if strings.Contains(arg, "--api-key") || strings.Contains(arg, "fixture-key") || strings.Contains(arg, "ignored-chart-user") {
							t.Error("credentials found in workload argv")
						}
					}
				}
			}
			wantSecrets := 1
			if tc.external != "" {
				wantSecrets = 0
			}
			if workloads != 2 || secrets != wantSecrets {
				t.Fatalf("workloads=%d Secrets=%d", workloads, secrets)
			}
		})
	}
}
