// Package acmedns adapts Lego DNS providers to a bounded, private worker process.
// The worker has the daemon's privileges; it isolates SDK output and lifetimes,
// not filesystem/network authority. Credentials use Lego's environment contract.
package acmedns

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const WorkerArgument = "--internal-acme-dns-worker"

// Some providers wait for asynchronous API changes during Present and CleanUp.
const operationTimeout = 5 * time.Minute
const cleanupTimeout = operationTimeout
const initializationTimeout = 30 * time.Second

type Failure string

func (e Failure) Error() string          { return "acme: " + string(e) }
func (e Failure) DNSFailureCode() string { return string(e) }

type request struct {
	Operation string    `json:"operation"`
	Provider  string    `json:"provider,omitempty"`
	Resolvers []string  `json:"resolvers,omitempty"`
	Domain    string    `json:"domain,omitempty"`
	Token     string    `json:"token,omitempty"`
	KeyAuth   string    `json:"key_auth,omitempty"`
	Deadline  time.Time `json:"deadline"`
}
type response struct {
	Error    string        `json:"error,omitempty"`
	Timeout  time.Duration `json:"timeout,omitempty"`
	Interval time.Duration `json:"interval,omitempty"`
}

// Session retains the provider instance, including provider-specific record IDs,
// until cleanup completes. Only application-owned error codes cross the pipe.
type Session struct {
	cmd               *exec.Cmd
	input             io.WriteCloser
	output            *os.File
	reader            *bufio.Reader
	closeOnce         sync.Once
	mu                sync.Mutex
	resolvers         []string
	timeout, interval time.Duration
}

func New(ctx context.Context, provider string, resolvers []string) (*Session, error) {
	if !Supported(provider) {
		return nil, Failure("dns_provider_unsupported")
	}
	// Linux keeps this executable available through an on-disk package upgrade.
	executable := "/proc/self/exe"
	s, err := startWorker(executable, os.Environ())
	if err != nil {
		return nil, err
	}
	s.resolvers = append([]string(nil), resolvers...)
	reply, err := s.call(ctx, request{Operation: "init", Provider: provider, Resolvers: resolvers}, initializationTimeout)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.timeout, s.interval = reply.Timeout, reply.Interval
	if s.timeout < time.Second || s.timeout > time.Hour || s.interval < time.Second || s.interval > 2*time.Minute {
		s.Close()
		return nil, Failure("dns_provider_configuration_failed")
	}
	return s, nil
}

func startWorker(executable string, env []string) (*Session, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, Failure("dns_worker_failed")
	}
	cmd := exec.Command(executable, WorkerArgument)
	cmd.Env = env
	// A dedicated response descriptor prevents provider/SDK stdout or stderr
	// (including debug dumps) from becoming protocol messages or daemon logs.
	cmd.ExtraFiles = []*os.File{write}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = time.Second
	input, err := cmd.StdinPipe()
	if err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, Failure("dns_worker_failed")
	}
	if err := cmd.Start(); err != nil {
		_ = input.Close()
		_ = read.Close()
		_ = write.Close()
		return nil, Failure("dns_worker_failed")
	}
	_ = write.Close()
	s := &Session{cmd: cmd, input: input, output: read, reader: bufio.NewReaderSize(read, 4096)}
	return s, nil
}

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		// Kill the group as well as the worker: a provider may use an SDK credential
		// helper. No helper may retain the protocol pipe or outlive this operation.
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		_ = s.cmd.Process.Kill()
		_ = s.input.Close()
		_ = s.output.Close()
		// Reap only after signalling the group, so its PID cannot be reused by an
		// unrelated process between Wait and Kill.
		_ = s.cmd.Wait()
	})
}

func (s *Session) call(ctx context.Context, req request, limit time.Duration) (response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return response{}, err
	}
	req.Deadline, _ = ctx.Deadline()
	type result struct {
		response response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		if err := json.NewEncoder(s.input).Encode(req); err != nil {
			done <- result{err: err}
			return
		}
		line, err := s.reader.ReadSlice('\n')
		if err != nil {
			done <- result{err: err}
			return
		}
		var reply response
		err = json.Unmarshal(line, &reply)
		done <- result{response: reply, err: err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			return response{}, Failure("dns_worker_failed")
		}
		switch result.response.Error {
		case "":
			return result.response, nil
		case "dns_provider_configuration_failed", "dns_provider_unsupported", "dns_present_failed", "dns_cleanup_failed", "dns_worker_failed":
			return response{}, Failure(result.response.Error)
		default:
			return response{}, Failure("dns_worker_failed")
		}
	case <-ctx.Done():
		s.Close()
		<-done
		return response{}, ctx.Err()
	}
}

func (s *Session) Present(ctx context.Context, domain, token, keyAuth string) error {
	_, err := s.call(ctx, request{Operation: "present", Domain: domain, Token: token, KeyAuth: keyAuth}, operationTimeout)
	return err
}
func (s *Session) CleanUp(ctx context.Context, domain, token, keyAuth string) error {
	_, err := s.call(ctx, request{Operation: "cleanup", Domain: domain, Token: token, KeyAuth: keyAuth}, cleanupTimeout)
	return err
}

// Wait requires the expected value at every configured recursive resolver.
// Fully-qualified queries preserve CNAME delegation and avoid search domains.
// With no explicit resolver, the system's Go DNS resolver is used. Authorities
// still perform their independent validation; split DNS needs public resolvers.
func (s *Session) Wait(ctx context.Context, domain, _, keyAuth string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	sum := sha256.Sum256([]byte(keyAuth))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	resolvers := s.resolvers
	if len(resolvers) == 0 {
		resolvers = []string{""}
	}
	for {
		ready := true
		for _, address := range resolvers {
			queryCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			resolver := &net.Resolver{PreferGo: true}
			if address != "" {
				resolver.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
				}
			}
			records, err := resolver.LookupTXT(queryCtx, "_acme-challenge."+domain+".")
			stop()
			found := false
			for _, record := range records {
				if record == want {
					found = true
					break
				}
			}
			if err != nil || !found {
				ready = false
				break
			}
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(s.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return Failure("dns_propagation_timeout")
		case <-timer.C:
		}
	}
}
