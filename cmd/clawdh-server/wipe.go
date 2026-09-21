package main

import (
	"context"
	"fmt"

	"clawdh/panelpg"
)

// runWipeUsage clears the metering the gateway has recorded — every usage
// event and hourly counter, and each login's window reading — so the boards
// start over. Shares, ceilings and the panel's own state are untouched. It
// prints what each table holds and stops there unless told to go ahead, so a
// wipe is never a surprise; the poller re-reads each login's windows within
// minutes and the next forwarded request starts the new record.
func runWipeUsage(ctx context.Context, dsn string, yes bool) error {
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}
	pg, err := panelpg.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pg.Close()

	show := func(when string) error {
		counts, err := pg.MeteringCounts(ctx)
		if err != nil {
			return err
		}
		fmt.Println(when + ":")
		for _, t := range panelpg.MeteringTables() {
			fmt.Printf("  %-16s %8d rows\n", t, counts[t])
		}
		return nil
	}
	if err := show("metering now"); err != nil {
		return err
	}
	if !yes {
		fmt.Println("\nnothing cleared. Run `clawdh-server wipe-usage --yes` to clear these tables.")
		return nil
	}
	if err := pg.WipeMetering(ctx); err != nil {
		return err
	}
	fmt.Println()
	return show("metering after the wipe")
}
