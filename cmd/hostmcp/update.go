package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/soren-labs/hostmcp/internal/update"
)

func updateCommand(ctx context.Context, args []string) error {
	f := flags("agent update")
	policyFile := f.String("policy", "", "private, locally provisioned update policy JSON")
	archiveFile := f.String("archive", "", "offline release archive; must match policy checksum")
	rollback := f.Bool("rollback", false, "restore the locally retained previous binary")
	if err := parse(f, args); err != nil {
		return err
	}
	if *policyFile == "" {
		return errors.New("--policy is required (updates are opt-in)")
	}
	p, err := update.Load(*policyFile)
	if err != nil {
		return err
	}
	if *rollback {
		if *archiveFile != "" {
			return errors.New("--rollback cannot accept an archive")
		}
		if err = update.Rollback(ctx, p); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Restored previous agent binary; restart the existing agent service and verify list_devices.")
		return nil
	}
	current, err := update.Inspect(ctx, p)
	if err != nil {
		return err
	}
	if current.Version == p.TargetVersion {
		fmt.Fprintln(os.Stderr, "Agent is already at the pinned version.")
		return nil
	}
	var archive io.ReadCloser
	if *archiveFile != "" {
		archive, err = os.Open(*archiveFile)
	} else {
		archive, err = update.Download(ctx, p)
	}
	if err != nil {
		return err
	}
	defer archive.Close()
	changed, err := update.Apply(ctx, p, archive)
	if changed {
		fmt.Fprintln(os.Stderr, "Agent binary replaced; previous binary retained. Restart the existing agent service and verify list_devices; managed sessions end on restart.")
	}
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintln(os.Stderr, "Agent is already at the pinned version.")
	}
	return nil
}
