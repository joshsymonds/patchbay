package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/server"
)

// documentLimitBytes caps the introspection document the CLI will hold. The
// document is a summary of a registry file; anything past this ceiling is not
// one, and is refused rather than decoded.
const documentLimitBytes = 4 << 20

// daemon is a running patchbay as the CLI addresses it: where it listens, and
// the caller key this process presents to prove it may ask.
type daemon struct {
	addr   string
	key    string
	client *http.Client
}

// dial resolves how to reach the running gateway. It fails before any request
// when this process cannot read the caller key at all, because that is a
// misconfiguration here rather than a refusal there — and the two deserve
// different messages.
func (c cli) dial() (daemon, error) {
	key, err := proxy.CallerKey()
	if err != nil {
		return daemon{}, fmt.Errorf("the caller key is unavailable: %w", err)
	}

	client := c.client
	if client == nil {
		client = &http.Client{}
	}

	return daemon{
		addr:   valueOr(c.lookupEnv, envListen, defaultListen),
		key:    key,
		client: client,
	}, nil
}

// document asks the gateway to describe its own routing. Every failure names
// the address that was tried, because the first thing an operator needs to know
// is which patchbay did not answer.
func (d daemon) document(ctx context.Context) (server.Document, error) {
	var doc server.Document

	target := "http://" + d.addr + server.RegistryEndpoint

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return doc, fmt.Errorf("build a request to %s: %w", d.addr, err)
	}

	req.Header.Set(proxy.CallerKeyHeader, d.key)

	resp, doErr := d.client.Do(req)
	if doErr != nil {
		return doc, fmt.Errorf("no gateway answered at %s — is patchbay running? %w", d.addr, doErr)
	}

	defer resp.Body.Close()

	if statusErr := d.checkStatus(resp.StatusCode); statusErr != nil {
		return doc, statusErr
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, documentLimitBytes+1))
	if readErr != nil {
		return doc, fmt.Errorf("read the answer from %s: %w", d.addr, readErr)
	}

	if len(body) > documentLimitBytes {
		return doc, fmt.Errorf("the gateway at %s answered with more than %d bytes", d.addr, documentLimitBytes)
	}

	if decodeErr := json.Unmarshal(body, &doc); decodeErr != nil {
		return doc, fmt.Errorf("the gateway at %s answered with something that is not a registry document: %w",
			d.addr, decodeErr)
	}

	return doc, nil
}

// checkStatus turns the gateway's status into the failure it describes. A
// refusal is called out by name: the one thing that goes wrong here in practice
// is two different caller-key files, and nothing else would say so.
func (d daemon) checkStatus(status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusForbidden:
		return fmt.Errorf(
			"the gateway at %s refused the caller key: the file named by %s does not match the one it reads",
			d.addr, proxy.CallerKeyEnvFile)
	case http.StatusNotFound:
		return fmt.Errorf("the gateway at %s does not serve %s; it is an older patchbay",
			d.addr, server.RegistryEndpoint)
	default:
		return fmt.Errorf("the gateway at %s answered %d", d.addr, status)
	}
}

// contextSummary returns one context from a document, or an error naming what
// the gateway does serve — the CLI never falls back to another context, for the
// same reason the gateway never does.
func contextSummary(doc server.Document, name string) (server.ContextSummary, error) {
	summary, ok := doc.Contexts[name]
	if !ok {
		return server.ContextSummary{}, fmt.Errorf("no context named %q is served; the gateway serves: %s",
			name, strings.Join(contextNames(doc), ", "))
	}

	return summary, nil
}
