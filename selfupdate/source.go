package selfupdate

import (
	"context"

	"github.com/wjordan/go-binsync/agent"
	"github.com/wjordan/go-binsync/release"
)

// agentSource is the embedded shape's update source: go-binsync/agent, with the
// exec handoff as its restart hook.
//
// The agent hands its hook no release hash -- an external agent restarts a
// service that has already been replaced on disk and has no use for one --
// but the handoff does: it must record the release as failed if the new
// process never reports ready. The hash is the one the installer wrote into
// the pending marker moments earlier, which is the release the hook is being
// asked to activate.
type agentSource struct {
	cfg  agent.Config
	inst *release.Installer
}

func (s *agentSource) Run(ctx context.Context, restart func(release.Hash) error) error {
	return agent.Loop(ctx, s.cfg, agent.Hooks{
		Restart: func(ctx context.Context) error {
			h, _, err := s.inst.Pending()
			if err != nil {
				return err
			}
			return restart(h)
		},
	})
}
