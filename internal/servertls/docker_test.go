package servertls

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/certfixture"
	"golang.org/x/crypto/bcrypt"
)

// TestContainerHTTPSHealthcheck checks the optional curl image with generated
// local PKI. The disposable credentials and keys below have no outside use.
func TestContainerHTTPSHealthcheck(t *testing.T) {
	image := os.Getenv("ACU_TLS_SMOKE_IMAGE")
	if image == "" {
		t.Skip("set ACU_TLS_SMOKE_IMAGE to the built docker/Dockerfile.acme image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0444); err != nil {
			t.Fatal(err)
		}
	}
	pair := certfixture.New(t, func(c *x509.Certificate) { c.DNSNames = []string{"cache.example.net"}; c.IPAddresses = nil })
	write("cert.pem", pair.Cert)
	write("key.pem", pair.Key)
	hash, err := bcrypt.GenerateFromPassword([]byte("smoke-only-password"), 4)
	if err != nil {
		t.Fatal(err)
	}
	write("admin.htpasswd", []byte("smoke:"+string(hash)+"\n"))
	write("healthcheck.conf", []byte("url = \"https://cache.example.net:6789/healthz\"\nresolve = \"cache.example.net:6789:127.0.0.1\"\ncacert = \"/run/fixture/cert.pem\"\nuser = \"smoke:smoke-only-password\"\n"))
	write("config.toml", []byte(`[cache]
dir = "/var/cache/apt-cacher-ultra"
listen = "127.0.0.1:3142"
listen_tls = "127.0.0.1:3143"
tls_cert = "/run/fixture/cert.pem"
tls_key = "/run/fixture/key.pem"
[admin]
listen = "127.0.0.1:6789"
tls_cert = "/run/fixture/cert.pem"
tls_key = "/run/fixture/key.pem"
htpasswd_file = "/run/fixture/admin.htpasswd"
[tls_mitm]
enabled = false
`))
	name := "acu-tls-smoke-" + strings.ToLower(rand.Text())
	run := func(args ...string) string {
		t.Helper()
		b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", args[0], err, b)
		}
		return strings.TrimSpace(string(b))
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if t.Failed() {
			b, _ := exec.CommandContext(cleanup, "docker", "logs", name).CombinedOutput()
			t.Logf("daemon: %s", b)
		}
		_ = exec.CommandContext(cleanup, "docker", "rm", "-f", name).Run()
	})
	run("run", "-d", "--name", name, "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--mount", fmt.Sprintf("type=bind,source=%s,target=/run/fixture,readonly", dir),
		"--tmpfs", "/var/cache/apt-cacher-ultra:uid=10001,gid=10001,mode=0700",
		"--health-cmd", "curl --fail --silent --show-error --max-time 4 --config /run/fixture/healthcheck.conf",
		"--health-interval", "1s", "--health-timeout", "5s", "--health-start-period", "1s", image, "-config", "/run/fixture/config.toml")
	wait := func() {
		t.Helper()
		for {
			state := run("inspect", "--format", "{{.State.Running}} {{.State.Health.Status}}", name)
			if state == "true healthy" {
				return
			}
			if strings.HasPrefix(state, "false") {
				t.Fatal("daemon exited")
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Second):
			}
		}
	}
	wait()
	if got := run("exec", name, "id", "-u"); got != "10001" {
		t.Fatalf("unexpected UID %s", got)
	}
	status := run("exec", name, "curl", "--silent", "--show-error", "--cacert", "/run/fixture/cert.pem", "--resolve", "cache.example.net:6789:127.0.0.1", "--output", "/dev/null", "--write-out", "%{http_code}", "https://cache.example.net:6789/healthz")
	if status != "401" {
		t.Fatalf("unauthenticated healthcheck: %s", status)
	}
	// Verify the independent cache TLS endpoint against the same trusted root.
	run("exec", name, "curl", "--silent", "--show-error", "--cacert", "/run/fixture/cert.pem", "--resolve", "cache.example.net:3143:127.0.0.1", "--output", "/dev/null", "https://cache.example.net:3143/")
	run("restart", name)
	wait()
}
