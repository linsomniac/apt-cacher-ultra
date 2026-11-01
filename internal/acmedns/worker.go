package acmedns

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/go-acme/lego/v5/challenge"
	"github.com/go-acme/lego/v5/challenge/dns01"
	legolog "github.com/go-acme/lego/v5/log"
	"github.com/go-acme/lego/v5/providers/dns"
)

// Providers excludes interactive/manual operations and arbitrary command hooks
// from the unattended daemon. The other compiled Lego providers are available.
func Providers() []string {
	out := slices.DeleteFunc(dns.GetProviderCodes(), func(s string) bool { return s == "manual" || s == "exec" })
	slices.Sort(out)
	return out
}
func canonical(name string) string {
	switch name {
	case "rfc2136":
		return "dnsupdate"
	case "acme-dns":
		return "acmedns"
	}
	return name
}
func Supported(name string) bool { return slices.Contains(Providers(), canonical(name)) }

// RunWorker is entered only by the executable's private worker argument. The
// parent passes descriptor 3; nothing private is accepted on the command line.
func RunWorker() int {
	syscall.CloseOnExec(3)
	output := os.NewFile(3, "acme-dns-response")
	if output == nil {
		return 1
	}
	defer func() { _ = output.Close() }()
	legolog.SetDefault(slog.New(slog.DiscardHandler))
	slog.SetDefault(slog.New(slog.DiscardHandler))
	log.SetOutput(io.Discard)
	return serveWorker(os.Stdin, output, dns.NewDNSChallengeProviderByName)
}

var dnsDomain = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
var authorization = regexp.MustCompile(`^[A-Za-z0-9_-]{16,256}\.[A-Za-z0-9_-]{43}$`)

func serveWorker(input io.Reader, output io.Writer, create func(string) (challenge.Provider, error)) int {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 16<<10)
	encoder := json.NewEncoder(output)
	var provider challenge.Provider
	var presented *request
	for scanner.Scan() {
		var req request
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			return 1
		}
		if req.Deadline.IsZero() || time.Until(req.Deadline) > operationTimeout+time.Second {
			return 1
		}
		ctx, cancel := context.WithDeadline(context.Background(), req.Deadline)
		reply := response{}
		switch req.Operation {
		case "init":
			if provider != nil || !Supported(req.Provider) {
				reply.Error = "dns_provider_unsupported"
				break
			}
			options := dns01.NewOptions()
			options.RecursiveNameservers = req.Resolvers
			options.Timeout = 3 * time.Second
			dns01.SetDefaultClient(dns01.NewClient(options))
			var err error
			provider, err = create(canonical(req.Provider))
			if err != nil || provider == nil {
				reply.Error = "dns_provider_configuration_failed"
				break
			}
			reply.Timeout, reply.Interval = 2*time.Minute, 2*time.Second
			if timed, ok := provider.(challenge.ProviderTimeout); ok {
				reply.Timeout, reply.Interval = timed.Timeout()
			}
		case "present":
			if provider == nil || presented != nil || !dnsDomain.MatchString(req.Domain) || strings.Contains(req.Domain, "..") || !authorization.MatchString(req.KeyAuth) || !strings.HasPrefix(req.KeyAuth, req.Token+".") {
				reply.Error = "dns_worker_failed"
				break
			}
			// Retain the attempt even after a provider error: the write may have
			// succeeded remotely before its response was lost.
			copy := req
			presented = &copy
			if err := provider.Present(ctx, req.Domain, req.Token, req.KeyAuth); err != nil {
				reply.Error = "dns_present_failed"
			}
		case "cleanup":
			if provider == nil || presented == nil {
				reply.Error = "dns_cleanup_failed"
				break
			}
			if err := provider.CleanUp(ctx, presented.Domain, presented.Token, presented.KeyAuth); err != nil {
				reply.Error = "dns_cleanup_failed"
			} else {
				presented = nil
			}
		default:
			reply.Error = "dns_worker_failed"
		}
		cancel()
		if encoder.Encode(reply) != nil {
			return 1
		}
	}
	if scanner.Err() != nil {
		return 1
	}
	return 0
}
