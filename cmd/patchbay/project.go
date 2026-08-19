package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
)

// The Claude Code settings chain, most local first. A setting in the local file
// overrides the project file, which overrides the user's own — the documented
// precedence, and the reason a directory can select a context nobody reading
// the project's checked-in settings would expect.
const (
	claudeDir         = ".claude"
	settingsLocalName = "settings.local.json"
	settingsName      = "settings.json"
)

// baseURLKey is the environment variable a settings file sets to point Claude
// Code at patchbay, under the file's own "env" object.
const baseURLKey = "ANTHROPIC_BASE_URL"

// claudeEndpoint is the path Claude Code appends to whatever base URL it is
// given. The context a directory ends up in is the one the gateway would read
// out of THAT path, not out of the base URL alone — which is what makes a base
// URL ending at the bare "/ctx" select the context "v1".
const claudeEndpoint = "/v1/messages"

// settingsSource is one file in the chain: where it is, what it had to say
// about the base URL, and the base URL itself when it set one.
type settingsSource struct {
	path    string
	note    string
	baseURL string
}

// settingsFile is the little of a Claude Code settings file this reads. Every
// other key in it belongs to Claude Code and is none of patchbay's business.
type settingsFile struct {
	Env map[string]string `json:"env"`
}

// project reports which context a directory's Claude Code settings select, and
// what that context will do with a request.
func (c cli) project(ctx context.Context, args []string) int {
	flags := c.flagSet(cmdProject)
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}

	if flags.NArg() > 1 {
		c.usage(c.stderr)

		return exitUsage
	}

	dir, dirErr := c.projectDir(flags.Arg(0))
	if dirErr != nil {
		c.fail(dirErr)

		return exitFailure
	}

	reached, dialErr := c.dial()
	if dialErr != nil {
		c.fail(dialErr)

		return exitFailure
	}

	base := c.writeChain(dir)
	name, code := c.selectedContext(base, reached.addr)

	writeLine(c.stdout, "context", name)

	doc, docErr := reached.document(ctx)
	if docErr != nil {
		c.fail(docErr)

		return exitFailure
	}

	if routesErr := c.writeRoutes(doc, name); routesErr != nil {
		c.noticef("WARNING: the settings select a context this gateway does not serve: %v", routesErr)

		return exitFailure
	}

	return code
}

// projectDir resolves the directory to inspect, which defaults to the one the
// command was run in.
func (c cli) projectDir(arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}

	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve the working directory: %w", err)
	}

	return dir, nil
}

// writeChain prints every settings file consulted, in precedence order, and
// returns the base URL that wins. A file that is missing, unreadable or not
// JSON is reported and skipped rather than ending the command: the rest of the
// chain still decides something, and an operator with one broken file needs to
// see what the others did.
func (c cli) writeChain(dir string) string {
	base := ""

	for _, source := range c.settingsChain(dir) {
		fmt.Fprintf(c.stdout, "settings %s: %s\n", source.path, source.note)

		if base == "" {
			base = source.baseURL
		}
	}

	if base == "" {
		writeLine(c.stdout, "base_url", "none — no settings file sets "+baseURLKey)

		return ""
	}

	writeLine(c.stdout, "base_url", base)

	return base
}

// settingsChain reads the three settings files, most local first.
func (c cli) settingsChain(dir string) []settingsSource {
	paths := []string{
		filepath.Join(dir, claudeDir, settingsLocalName),
		filepath.Join(dir, claudeDir, settingsName),
	}

	if home := value(c.lookupEnv, envHome); home != "" {
		paths = append(paths, filepath.Join(home, claudeDir, settingsName))
	} else {
		c.noticef("$%s is not set, so the user-level settings file was not consulted", envHome)
	}

	sources := make([]settingsSource, 0, len(paths))
	for _, path := range paths {
		sources = append(sources, c.readSettings(path))
	}

	return sources
}

// readSettings reads one settings file's base URL, if it sets one.
func (c cli) readSettings(path string) settingsSource {
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return settingsSource{path: path, note: "not present", baseURL: ""}
		}

		c.noticef("%s could not be read and was skipped: %v", path, err)

		return settingsSource{path: path, note: "unreadable", baseURL: ""}
	}

	var file settingsFile
	if decodeErr := json.Unmarshal(contents, &file); decodeErr != nil {
		c.noticef("%s is not valid JSON and was skipped: %v", path, decodeErr)

		return settingsSource{path: path, note: "malformed JSON, skipped", baseURL: ""}
	}

	base := file.Env[baseURLKey]
	if base == "" {
		return settingsSource{path: path, note: "no " + baseURLKey, baseURL: ""}
	}

	return settingsSource{path: path, note: baseURLKey + "=" + base, baseURL: base}
}

// selectedContext reports the context a base URL selects, and the exit code the
// command has earned so far. A base URL that names some other gateway is worth
// saying out loud — the routes printed below it are this gateway's, not that
// one's — but it is a notice, not a failure: pointing Claude Code somewhere
// else is a thing an operator may perfectly well have meant.
func (c cli) selectedContext(base, addr string) (string, int) {
	if base == "" {
		return registry.PersonalContext, exitOK
	}

	parsed, err := url.Parse(base)
	if err != nil {
		c.noticef("%s is not a URL (%v); reading it as the personal context", baseURLKey, err)

		return registry.PersonalContext, exitFailure
	}

	if parsed.Host != addr {
		c.noticef("%s points at %s, which is not the gateway this command asks (%s)", baseURLKey, parsed.Host, addr)
	}

	// Split by the gateway's own code, on the path a request to this base URL
	// would actually carry, so this can never diagnose a base URL differently
	// from the gateway that has to serve it.
	return server.ContextFromPath(strings.TrimSuffix(parsed.Path, "/") + claudeEndpoint), exitOK
}
