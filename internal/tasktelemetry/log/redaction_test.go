package tasklogs

import "testing"

func TestEnvironmentRedaction(t *testing.T) {
	for _, list := range []bool{false, true} {
		name := "map"
		if list {
			name = "list"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name, value, message, want string
			}{
				{"ENABLED", "1", "2026-10-18T19:58:17Z libapparmor1", "2026-10-18T19:58:17Z libapparmor1"},
				{"DISABLED", "0", "\x1b[0m version 4.0.1", "\x1b[0m version 4.0.1"},
				{"DEBUG", "true", "debug=true", "debug=true"},
				{"RETRY_DELAY", "0.5", "delay=0.5", "delay=0.5"},
				{"TASK_ID", "run-id", "task run-id", "task run-id"},
				{"OZ_RUN_ID", "run-id", "oz-task-run-id", "oz-task-run-id"},
				{"WARP_EXECUTION_ID", "execution-id", "execution-id", "execution-id"},
				{"WARP_SERVER_ROOT_URL", "https://staging.warp.dev", "https://staging.warp.dev", "https://staging.warp.dev"},
				{"PATH", "/usr/bin", "using /usr/bin", "using /usr/bin"},
				{"WARP_API_KEY", "1234", "key=1234", "key=[REDACTED]"},
				{"password", "false", "password=false", "password=[REDACTED]"},
				{"AUTH_TOKEN", "x", "token=x", "token=[REDACTED]"},
				{"CUSTOM", "custom-value", "custom-value", "[REDACTED]"},
				{"WARP_SERVER_ROOT_URL", "https://user:password@example.com", "https://user:password@example.com", "[REDACTED]"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r := &Reporter{}
					if list {
						r.AddEnvList([]string{tc.name + "=" + tc.value})
					} else {
						r.AddEnv(map[string]string{tc.name: tc.value})
					}
					if got := r.redactor.redact(tc.message); got != tc.want {
						t.Errorf("structured redaction = %q, want %q", got, tc.want)
					}
					if got := r.redactor.redactLine(tc.message); got != tc.want {
						t.Errorf("raw redaction = %q, want %q", got, tc.want)
					}
				})
			}
		})
	}
}
