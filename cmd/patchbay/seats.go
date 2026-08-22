package main

import (
	"context"
	"fmt"
)

func (c cli) seats(ctx context.Context, args []string) int {
	flags := c.flagSet(cmdSeats)
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != 0 {
		c.usage(c.stderr)
		return exitUsage
	}
	doc, err := c.fetch(ctx)
	if err != nil {
		c.fail(err)
		return exitFailure
	}
	table := c.table()
	fmt.Fprintln(table, "SEAT\tAUTH_MODE\tUPSTREAM\tMODEL\tMAX_INPUT_TOKENS\tCONFIG_SOURCE")
	for _, id := range seatNames(doc) {
		seat := doc.Seats[id]
		source := callerCredential
		if seat.APIKeyEnvFile != "" {
			source = seat.APIKeyEnvFile
		} else if seat.AWSProfile != "" {
			source = "aws_profile:" + seat.AWSProfile
		}
		fmt.Fprintf(
			table,
			"%s\t%s\t%s\t%s\t%s\t%s\n",
			quoted(id),
			quoted(seat.AuthMode),
			quoted(seat.Upstream),
			nullable(seat.Model),
			tokenCeiling(seat.MaxInputTokens),
			quoted(source),
		)
	}
	c.flush(table)
	return exitOK
}
