package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
)

// resolveArgs is how many arguments resolve takes: the context, and the model.
const resolveArgs = 2

// resolve reports what one request would actually do: which route serves it,
// where that route goes and how it pays.
func (c cli) resolve(ctx context.Context, args []string) int {
	flags := c.flagSet(cmdResolve)
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}

	if flags.NArg() != resolveArgs {
		c.usage(c.stderr)

		return exitUsage
	}

	name, model := flags.Arg(0), flags.Arg(1)

	doc, err := c.fetch(ctx)
	if err != nil {
		c.fail(err)

		return exitFailure
	}

	summary, contextErr := contextSummary(doc, name)
	if contextErr != nil {
		c.fail(contextErr)

		return exitFailure
	}

	c.writeResolution(name, model, summary)

	return exitOK
}

// writeResolution prints one request's fate, one record per line.
func (c cli) writeResolution(name, model string, summary server.ContextSummary) {
	route, matched := summary.Routes[model]
	origin := "alias " + model

	if !matched {
		route, origin = summary.DefaultRoute, "default route"
	}

	writeLine(c.stdout, "context", name)
	writeLine(c.stdout, "request_model", model)
	writeLine(c.stdout, "route", origin)
	writeLine(c.stdout, "auth", route.Auth)
	writeLine(c.stdout, "upstream", route.Upstream)
	writeLine(c.stdout, "model", upstreamModel(route, model))
	writeLine(c.stdout, "credential", credential(route))

	if route.Auth == string(registry.AuthSigV4) {
		c.writeMapping(route, upstreamModel(route, model))
	}
}

// writeMapping reports what the Bedrock model map has to say about this
// request. The lookup itself belongs to the gateway, which does it per request
// against the file as it stands then — so this is a reading of the same file
// from here, and says so. The mapped Bedrock id is never printed: it is the one
// thing the map file exists to keep out of the registry.
func (c cli) writeMapping(route server.RouteSummary, model string) {
	writeLine(c.stdout, "model_map",
		fmt.Sprintf("%s=%s (the gateway maps the model on every request)",
			route.ModelMapEnvFile, orAbsent(route.ModelMapFile)))

	if route.ModelMapFile == "" {
		writeLine(c.stdout, "mapping", "unknown: the gateway reported no model map path")

		return
	}

	mapping, err := readModelMap(route.ModelMapFile)
	if err != nil {
		writeLine(c.stdout, "mapping", fmt.Sprintf("unknown: %v", err))

		return
	}

	if target := mapping[model]; target == "" {
		writeLine(c.stdout, "mapping",
			fmt.Sprintf("%q is not mapped — the gateway would refuse this request with 400", model))

		return
	}

	writeLine(c.stdout, "mapping", fmt.Sprintf("%q is mapped", model))
}

// readModelMap reads a Bedrock model map from disk. It is only ever consulted
// to answer whether a model has an entry; no value it holds leaves this
// function.
func readModelMap(path string) (map[string]string, error) {
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("the model map is not readable from here: %w", err)
	}

	var mapping map[string]string
	if decodeErr := json.Unmarshal(contents, &mapping); decodeErr != nil {
		return nil, fmt.Errorf("the model map at %s is not a JSON object of model ids: %w", path, decodeErr)
	}

	return mapping, nil
}

// upstreamModel is the model the upstream is actually asked for: the route's
// rewrite target, or — when it rewrites nothing — the model the caller named.
func upstreamModel(route server.RouteSummary, requested string) string {
	if route.Model != "" {
		return route.Model
	}

	return requested
}

// credential names what pays for this request. An inject route names the file
// it reads the key from; a sigv4 route pays with credentials the gateway
// exports from its AWS profile, which is not a file this side can point at; a
// forward route spends whatever the caller itself presented.
func credential(route server.RouteSummary) string {
	switch route.Auth {
	case string(registry.AuthInject):
		return fmt.Sprintf("%s=%s", route.KeyEnvFile, orAbsent(route.KeyFile))
	case string(registry.AuthSigV4):
		return "aws credentials the gateway exports for this route's profile"
	default:
		return "the caller's own credentials"
	}
}
