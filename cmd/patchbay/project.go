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

	doc, docErr := reached.document(ctx)
	if docErr != nil {
		c.fail(docErr)
		return exitFailure
	}
	if !doc.RegistryValid {
		c.fail(fmt.Errorf("registry invalid: %s", doc.RegistryError))
		return exitFailure
	}
	if doc.DefaultContext == nil {
		c.fail(errors.New("gateway reported no default_context"))
		return exitFailure
	}

	base := c.writeChain(dir)

	// A base URL that selects nothing has already been reported on stderr, and
	// the settings chain above stands as the diagnosis. Everything below this
	// point describes a Context, so on a failed selection there is nothing left
	// to say: printing one anyway would describe this gateway's default as
	// though the project reached it.
	name, code := c.selectedContext(base, reached.addr, *doc.DefaultContext)
	if code != exitOK {
		return code
	}

	writeLine(c.stdout, "context", quoted(name))
	if _, contextErr := contextSummary(doc, name); contextErr != nil {
		c.noticef(
			"WARNING: the settings select a context this gateway does not serve: %v",
			contextErr,
		)
		return exitFailure
	}
	if bindingsCode := c.writeBindings(doc, name); bindingsCode != nil {
		c.fail(bindingsCode)
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
//
// The path and the base URL are quoted, because neither is patchbay's text. A
// settings file belongs to whoever checked the project out, and a base URL
// carrying a newline written back raw would append lines of its own to a
// diagnosis an operator reads as this command's. The fixed notes are patchbay's
// own words and stay bare, so "not present" is still readable as a verdict
// rather than as something a file said.
func (c cli) writeChain(dir string) string {
	base := ""

	for _, source := range c.settingsChain(dir) {
		fmt.Fprintf(c.stdout, "settings %s: %s\n", quoted(source.path), source.note)

		if base == "" {
			base = source.baseURL
		}
	}

	if base == "" {
		c.noticef("no settings file sets %s", baseURLKey)
	}

	writeLine(c.stdout, "base_url", nullable(base))

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

	return settingsSource{path: path, note: baseURLKey + "=" + quoted(base), baseURL: base}
}

// selectedContext reports the context a base URL selects, and the exit code the
// command has earned so far. A failing code comes with no context at all: the
// caller must print nothing that would describe one.
//
// ANTHROPIC_BASE_URL is the address Claude Code will actually dial, so it has to
// be an absolute http or https URL naming a host. A relative path, a hostless
// URL, or a scheme Claude Code will never speak selects no context at all —
// naming this gateway's default anyway would tell an operator the project is
// wired up when nothing will reach patchbay.
//
// A base URL that names some OTHER gateway is the one case worth only saying out
// loud — the bindings printed below it are this gateway's, not that one's — but
// pointing Claude Code somewhere else is a thing an operator may perfectly well
// have meant.
func (c cli) selectedContext(base, addr, defaultContext string) (string, int) {
	if base == "" {
		return defaultContext, exitOK
	}

	parsed, err := url.Parse(base)
	if err != nil {
		c.noticef("%s is not a URL (%v), so it selects no context", baseURLKey, err)

		return "", exitFailure
	}

	if !isDialable(parsed) {
		c.noticef(
			"%s %q is not an absolute http or https URL with a host, so it reaches no gateway and selects no context",
			baseURLKey,
			base,
		)

		return "", exitFailure
	}

	if parsed.Host != addr {
		c.noticef(
			"%s points at %s, which is not the gateway this command asks (%s)",
			baseURLKey,
			parsed.Host,
			addr,
		)
	}

	// Split by the gateway's own code, on the URL a request to this base URL
	// would actually carry, so this can never diagnose a base URL differently
	// from the gateway that has to serve it.
	target, targetErr := claudeTarget(parsed)
	if targetErr != nil {
		c.noticef(
			"%s %q does not name a path Claude Code could request (%v), so it selects no context",
			baseURLKey,
			base,
			targetErr,
		)

		return "", exitFailure
	}

	name := server.ContextFromURL(target, defaultContext)

	// The splitter answers with the empty name for a URL that named a context and
	// named it nothing: a prefix with no segment after it, or a segment that will
	// not come cleanly off both halves of an escaped path. No registry can produce
	// that name and the gateway refuses it, so this is a base URL that selects
	// nothing — not one that selects a context the gateway happens to be missing.
	if name == "" {
		c.noticef(
			"%s %q names a context and names it nothing — the prefix has no segment after it, "+
				"or the segment will not come cleanly off the escaped path — so the gateway "+
				"refuses it rather than serving another context",
			baseURLKey,
			base,
		)

		return "", exitFailure
	}

	return name, exitOK
}

// claudeTarget is the URL Claude Code will actually request: the configured base
// URL with the Messages endpoint appended.
//
// The escaped path is the single source of truth. The gateway routes on it and
// strips a context off both halves of the path it names, so the two halves have
// to stay one statement of the same thing: RawPath is a hint EscapedPath honors
// only while it encodes Path, and a URL whose halves disagree quietly loses the
// escaped one. Building each half from its own base half is how they come to
// disagree — a base URL ending at "/ctx/work%2F" has a decoded path ending in a
// separator and an escaped path that does not, so trimming both leaves an
// escaped path that no longer encodes the decoded one, and the whole diagnosis
// falls back to a context the request never reaches.
//
// So the endpoint is appended once, to the escaped path, and the decoded half is
// derived from the result rather than trimmed alongside it. One trailing slash
// still comes off, because that is a separator; "%2F" is a path byte and stays.
func claudeTarget(base *url.URL) (*url.URL, error) {
	escaped := strings.TrimSuffix(base.EscapedPath(), "/") + claudeEndpoint

	// EscapedPath only ever returns a valid escaping, so this cannot fail — but a
	// silently discarded decode would put the two halves right back out of step.
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, fmt.Errorf("decode the escaped path %q: %w", escaped, err)
	}

	target := *base
	target.Path = decoded
	target.RawPath = ""

	// RawPath is kept only when the default encoding of Path would say something
	// else, which is the same rule net/url applies when it parses one.
	if target.EscapedPath() != escaped {
		target.RawPath = escaped
	}

	return &target, nil
}

// isDialable reports whether target is something Claude Code can actually send a
// request to: an absolute URL, in a scheme it speaks, naming a host.
func isDialable(target *url.URL) bool {
	if !target.IsAbs() || target.Host == "" {
		return false
	}

	return target.Scheme == "http" || target.Scheme == "https"
}
