package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cliconfig "github.com/docker/cli/cli/config"
	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/warpdotdev/oz-agent-worker/internal/common"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestSidecarImageMappingsBackends(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	remapper, err := common.NewImageRemapper(map[string]string{
		"warpdotdev/warp-agent":              "mirror.example/agent",
		"warpdotdev/warp-claude-cli-sidecar": "mirror.example/claude:approved",
		"warpdotdev/warp-xvfb-sidecar":       "mirror.example/computer-use@" + digest,
		"ubuntu":                             "mirror.example/not-a-sidecar",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"docker", "kubernetes-copy", "kubernetes-image-volume"} {
		t.Run(mode, func(t *testing.T) {
			assignment := &types.TaskAssignmentMessage{
				TaskID:       "task-1",
				Task:         &types.Task{ID: "task-1"},
				DockerImage:  "ubuntu:22.04",
				SidecarImage: "docker.io/warpdotdev/warp-agent:v123",
				AdditionalSidecars: []types.SidecarMount{
					{Image: "warpdotdev/warp-claude-cli-sidecar:v2", MountPath: "/mnt/claude-cli-sidecar"},
					{Image: "warpdotdev/warp-xvfb-sidecar:v3", MountPath: "/mnt/xvfb-sidecar"},
					{Image: "other.example/extra:v1", MountPath: "/mnt/extra"},
				},
			}
			original := append([]types.SidecarMount(nil), assignment.AdditionalSidecars...)
			w := &Worker{ctx: context.Background(), config: Config{SidecarImageMappings: remapper}}
			params := w.prepareTaskParams(assignment)
			want := append([]types.SidecarMount{{Image: "mirror.example/agent:v123", MountPath: "/agent"}}, original...)
			want[1].Image = "mirror.example/claude:approved"
			want[2].Image = "mirror.example/computer-use@" + digest
			if !reflect.DeepEqual(params.Sidecars, want) || !reflect.DeepEqual(assignment.AdditionalSidecars, original) {
				t.Fatalf("sidecars = %+v; assignment = %+v; want %+v without mutating assignment", params.Sidecars, assignment.AdditionalSidecars, want)
			}
			if params.DockerImage != assignment.DockerImage || assignment.SidecarImage != "docker.io/warpdotdev/warp-agent:v123" {
				t.Fatal("mapping changed the task image or main sidecar assignment")
			}

			if mode == "docker" {
				configDir := t.TempDir()
				oldDir := cliconfig.Dir()
				cliconfig.SetDir(configDir)
				t.Cleanup(func() { cliconfig.SetDir(oldDir) })
				config := fmt.Sprintf(`{"auths":{"mirror.example":{"auth":%q}}}`, base64.StdEncoding.EncodeToString([]byte("mirror-test:fixture")))
				if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				var pulled []string
				engine := &fakeDockerEngine{
					t: t, volumeFound: true,
					inspectResponses: []func(*http.Request) *http.Response{
						func(req *http.Request) *http.Response {
							return mockImageInspectResponse(t, req, "linux", "amd64", "sha256:fixture")
						},
					},
					pullResponse: func(req *http.Request) *http.Response {
						ref := req.URL.Query().Get("fromImage")
						if tag := req.URL.Query().Get("tag"); tag != "" {
							if strings.HasPrefix(tag, "sha256:") {
								ref += "@" + tag
							} else {
								ref += ":" + tag
							}
						}
						pulled = append(pulled, ref)
						if strings.HasPrefix(ref, "mirror.example/") {
							auth, err := authconfig.Decode(req.Header.Get("X-Registry-Auth"))
							if err != nil || auth.Username != "mirror-test" {
								t.Fatal("sidecar pull did not use destination registry credentials")
							}
						}
						return mockEngineResponse(req, http.StatusOK, "application/json", "{}\n")
					},
				}
				backend := newTestDockerBackend(t, engine, PullPolicyAlways)
				binds, err := backend.prepareSidecars(context.Background(), backend.dockerClient, params.Sidecars, nil)
				if err != nil {
					t.Fatal(err)
				}
				for i, sidecar := range want {
					if i >= len(pulled) || pulled[i] != sidecar.Image {
						t.Fatalf("Docker pulled %v, want sidecar %q at index %d", pulled, sidecar.Image, i)
					}
					if i >= len(binds) || !strings.HasSuffix(binds[i], ":"+sidecar.MountPath+":ro") {
						t.Fatalf("Docker binds = %v, missing original mount %q", binds, sidecar.MountPath)
					}
				}
				return
			}

			client := fake.NewSimpleClientset()
			stop := errors.New("captured task Job")
			var pod *corev1.PodSpec
			client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
				pod = &action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).Spec.Template.Spec
				return true, nil, stop
			})
			backend := &KubernetesBackend{
				config:    KubernetesBackendConfig{Namespace: "agents", UseImageVolumes: mode == "kubernetes-image-volume"},
				clientset: client,
			}
			if result := backend.ExecuteTask(context.Background(), params); !errors.Is(result.Error, stop) {
				t.Fatalf("ExecuteTask error = %v, want captured Job", result.Error)
			}
			for i, sidecar := range want {
				if mode == "kubernetes-copy" {
					if pod.InitContainers[i].Image != sidecar.Image {
						t.Fatalf("init container image = %q, want %q", pod.InitContainers[i].Image, sidecar.Image)
					}
				} else if image := pod.Volumes[i+1].Image; image == nil || image.Reference != sidecar.Image {
					t.Fatalf("image volume = %+v, want %q", image, sidecar.Image)
				}
				mount := pod.Containers[0].VolumeMounts[i+1]
				if mount.MountPath != sidecar.MountPath || !mount.ReadOnly {
					t.Fatalf("Kubernetes mount = %+v, want read-only %q", mount, sidecar.MountPath)
				}
			}
		})
	}
}

func TestSidecarImageMappingsApplyAfterLegacyOverrides(t *testing.T) {
	remapper, err := common.NewImageRemapper(map[string]string{
		"custom.example/agent":  "mirror.example/agent",
		"custom.example/claude": "mirror.example/claude:approved",
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := "claude"
	for _, mainImage := range []string{"warpdotdev/warp-agent:v1", ""} {
		t.Run(mainImage, func(t *testing.T) {
			w := &Worker{
				ctx: context.Background(),
				config: Config{
					SidecarImageMappings: remapper,
					Kubernetes: &KubernetesBackendConfig{
						SidecarImage:      "custom.example/agent:custom",
						CodingCLISidecars: map[string]string{"claude": "custom.example/claude:v2"},
					},
				},
			}
			assignment := &types.TaskAssignmentMessage{
				TaskID:             "task-1",
				Task:               &types.Task{ID: "task-1", AgentConfigSnapshot: &types.AmbientAgentConfig{Harness: &types.Harness{Type: &harness}}},
				SidecarImage:       mainImage,
				AdditionalSidecars: []types.SidecarMount{{Image: "custom.example/claude:server", MountPath: "/mnt/claude-cli-sidecar", ReadWrite: true}},
			}
			original, _ := json.Marshal(assignment)
			params := w.prepareTaskParams(assignment)
			cli := params.Sidecars[len(params.Sidecars)-1]
			if cli.Image != "mirror.example/claude:approved" || !cli.ReadWrite {
				t.Fatalf("coding CLI = %+v, want mapped legacy override preserving read/write", cli)
			}
			if mainImage != "" && params.Sidecars[0].Image != "mirror.example/agent:custom" {
				t.Fatalf("main image = %q, want mapped legacy override", params.Sidecars[0].Image)
			}
			after, _ := json.Marshal(assignment)
			if string(after) != string(original) {
				t.Fatal("preparation mutated the assignment")
			}
			assignment.AdditionalSidecars = nil
			params = w.prepareTaskParams(assignment)
			if cli := params.Sidecars[len(params.Sidecars)-1]; cli.Image != "mirror.example/claude:approved" {
				t.Fatalf("injected coding CLI was not remapped: %+v", cli)
			}
		})
	}
}
