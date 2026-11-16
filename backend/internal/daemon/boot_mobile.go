package daemon

import (
	"context"
	"log/slog"
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
	"github.com/aoagents/agent-orchestrator/backend/internal/push"
	awakesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/awake"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/systeminstall"
)

// bootAwakeHost keeps the real awake state machine while allowing its native
// effects and application ownership observations to be supplied independently.
type bootAwakeHost interface {
	awakesvc.Host
	AppAlive(context.Context, string, string) (bool, string, error)
}

// mobileBootEffects contains every native/network fallback used by the bridge
// during boot, restore, status, and shutdown. Factories are explicit even for an
// unsupported capability; e.g. KeepAwake may deliberately return nil.
type mobileBootEffects struct {
	PickLANHosts       func() []string
	PickTailscaleHosts func() []string
	ApplyServe         func(int) error
	ClearServe         func() error
	QueryTS            func() mobilebridge.TailscaleInfo
	ServeTarget        func() int
	ReapTunnel         func() error
	ResolveTunnel      func(bool) controllers.TunnelController
	KeepAwake          func() controllers.KeepAwakeController
	PushSender         func() push.Sender
	PublishRemoteHost  func(context.Context, *controllers.BridgeService)
}

func (e mobileBootEffects) validate() error {
	return validateBootFunctions("mobile", e)
}

func (e mobileBootEffects) apply(bridge *controllers.BridgeService) {
	bridge.PickLANHosts = e.PickLANHosts
	bridge.PickTailscaleHosts = e.PickTailscaleHosts
	bridge.ApplyServe = e.ApplyServe
	bridge.ClearServe = e.ClearServe
	bridge.QueryTS = e.QueryTS
	bridge.ServeTarget = e.ServeTarget
	bridge.ResolveTunnel = func() controllers.TunnelController { return e.ResolveTunnel(true) }
	bridge.KeepAwake = e.KeepAwake()
}

// productionMobileBootEffects constructs closures only. Calls with native
// effects happen in the shared lifecycle after the complete set is validated.
func productionMobileBootEffects(cfg config.Config, bridge *controllers.BridgeService, installer *systeminstall.Service, hostID string, log *slog.Logger) mobileBootEffects {
	tunnelPID := mobilebridge.TunnelPIDPath(cfg.DataDir)
	resolve := func(installIfNeeded bool) controllers.TunnelController {
		resolution := mobilebridge.ResolveCloudflared(mobilebridge.LocalCloudflaredLookup(cfg.DataDir, systeminstall.PinnedCloudflared))
		if resolution.NeedsInstall {
			if installIfNeeded {
				if state, err := mobilebridge.Load(bridge.ConfigPath); err == nil && state.AccountTokenHash != "" {
					installer.DownloadCloudflaredInBackground(bridge.ConnectorInstalled)
				}
			} else {
				log.Info("mobile remote access unavailable; cloudflared not installed", "rejectedSystemPath", resolution.SystemPath)
			}
			return nil
		}
		log.Info("mobile remote access available", "cloudflared", resolution.Path, "source", resolution.Source)
		deps := mobilebridge.ManagedTunnelDeps{Binary: resolution.Path, PIDPath: tunnelPID, Log: log}
		if cfg.CloudControlPlaneURL != "" && hostID != "" {
			deps.Fixed = mobilebridge.NewFixedTunnelSource(cfg.CloudControlPlaneURL, cfg.DataDir)
			deps.HostID = hostID
		}
		return mobilebridge.NewManagedTunnel(deps)
	}
	return mobileBootEffects{
		PickLANHosts:       mobilebridge.LocalPrivateIPv4s,
		PickTailscaleHosts: mobilebridge.LocalTailscaleIPv4s,
		ApplyServe:         func(port int) error { return mobilebridge.NewServe().Apply(context.Background(), port) },
		ClearServe:         func() error { return mobilebridge.NewServe().Clear(context.Background()) },
		QueryTS:            func() mobilebridge.TailscaleInfo { return mobilebridge.QueryTailscale(context.Background()) },
		ServeTarget:        func() int { return mobilebridge.NewServe().Target(context.Background()) },
		ReapTunnel: func() error {
			return mobilebridge.ReapStaleTunnel(tunnelPID, mobilebridge.IsLiveCloudflared, mobilebridge.KillProcess)
		},
		ResolveTunnel: resolve,
		KeepAwake: func() controllers.KeepAwakeController {
			if !mobilebridge.KeepAwakeSupported() {
				return nil
			}
			keepAwake := mobilebridge.NewKeepAwake(os.Getpid())
			keepAwake.SetLidController(mobilebridge.NewLidAwake(os.Getpid(), os.Getenv("AO_WAKE_HELPER"), cfg.DataDir))
			return keepAwake
		},
		PushSender: func() push.Sender { return push.NewExpoClient(os.Getenv("EXPO_ACCESS_TOKEN")) },
		PublishRemoteHost: func(ctx context.Context, bridge *controllers.BridgeService) {
			runRemoteHostAddressPublisher(ctx, bridge, cfg.CloudControlPlaneURL, log)
		},
	}
}

type bootMobileLAN interface {
	controllers.LANController
	UseDeviceCredentials(*mobilebridge.AccessDevices, *mobilebridge.PairingCodes)
}
