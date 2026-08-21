package main

import (
	"context"
	"strconv"

	"github.com/joshsymonds/patchbay/internal/registry"
)

const resolveArgs = 2

func (c cli) resolve(ctx context.Context, args []string) int {
	flags := c.flagSet(cmdResolve)
	subagent := flags.Bool("subagent", false, "resolve as positively marked subagent traffic")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != resolveArgs {
		c.usage(c.stderr)
		return exitUsage
	}
	doc, err := c.fetch(ctx)
	if err != nil {
		c.fail(err)
		return exitFailure
	}
	observationClass := "none"
	var observation *registry.SubagentObservation
	if *subagent {
		observationClass = "subagent"
		observation = &registry.SubagentObservation{AgentID: "patchbay-cli"}
	}
	res, err := registry.ResolveState(
		registryState(doc),
		registry.Selection{Name: flags.Arg(0), Explicit: true},
		flags.Arg(1),
		observation,
	)
	if err != nil {
		c.fail(err)
		return exitFailure
	}
	c.writeResolution(flags.Arg(1), observationClass, res)
	return exitOK
}

// writeResolution prints the whole non-secret resolution, one field per line.
//
// Every string is a quoted literal and every absent one is an unquoted null.
// The two are not interchangeable spellings of the same thing: a Seat that
// rewrites to a model an operator named "-" and a Seat that rewrites to nothing
// used to print the same character, and the point of a resolution is to say
// exactly which one this is. Booleans and numbers are neither, so they print
// bare.
func (c cli) writeResolution(
	requestModel, observationClass string,
	res registry.Resolution,
) {
	writeLine(c.stdout, "context", quoted(res.Context))
	writeLine(c.stdout, "request_model", quoted(requestModel))
	writeLine(c.stdout, "observation", quoted(observationClass))
	writeLine(c.stdout, "selection", quoted(string(res.Match.Kind)))
	writeLine(c.stdout, "selection_model", nullableString(res.Match.Model))
	writeLine(c.stdout, "seat", quoted(res.SeatID))
	writeLine(c.stdout, "auth_mode", quoted(string(res.Seat.AuthMode)))
	writeLine(c.stdout, "upstream", quoted(res.Seat.EffectiveUpstream()))
	writeLine(c.stdout, "model", nullable(upstreamModel(res.Seat, requestModel)))
	writeLine(c.stdout, "config_source", quoted(res.Seat.CredentialSource()))
	writeLine(c.stdout, "api_key_env_file", nullable(res.Seat.APIKeyEnvFile))
	writeLine(c.stdout, "aws_profile", nullable(res.Seat.AWSProfile))
	writeLine(c.stdout, "aws_region", nullable(res.Seat.AWSRegion))
	writeLine(c.stdout, "model_map_env_file", nullable(res.Seat.ModelMapEnvFile))
	writeLine(c.stdout, "max_input_tokens", tokenCeiling(res.Seat.MaxInputTokens))
	writeLine(c.stdout, "max_output_tokens", tokenCeiling(res.Seat.MaxOutputTokens))
	writeLine(c.stdout, "insecure", strconv.FormatBool(res.Seat.Insecure))
	writeLine(c.stdout, "strip_cache_control", strconv.FormatBool(res.Seat.StripCacheControl))
	writeLine(c.stdout, "capture_bodies", strconv.FormatBool(res.Seat.CaptureBodies))
}

func upstreamModel(seat registry.Seat, requested string) string {
	if seat.Model != "" {
		return seat.Model
	}
	return requested
}
