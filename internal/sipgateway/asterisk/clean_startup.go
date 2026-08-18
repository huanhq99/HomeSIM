package asterisk

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/sipgateway"
)

const (
	cleanStartupMarkerPrefix = "dj1_clean_"
	maxCleanStartupChannels  = 4096
	maxCleanStartupEvents    = 8192
)

type startupChannelClass uint8

const (
	startupChannelUnrelated startupChannelClass = iota
	startupChannelOwned
	startupChannelAmbiguous
)

// proveCleanStartup scans current channels while consuming conservative event
// windows, then returns the second UserEvent's PBX-clock timestamp. Asterisk
// does not promise cross-topic ordering between channel events and UserEvent,
// so the markers are not treated as queue barriers: Adapter.startIncoming also
// rejects every subsequently delivered channel whose PBX creationtime is not
// strictly after this cutoff. This method never creates, answers, hangs up,
// adopts, or deletes a channel.
func proveCleanStartup(
	ctx context.Context,
	cfg normalizedConfig,
	pbx pbxIdentity,
	ari *ariWSClient,
) (time.Time, error) {
	scanCtx, cancel := withMaxTimeout(ctx, cfg.httpTimeout)
	defer cancel()

	first, err := cleanStartupMarker(cfg.idReader)
	if err != nil {
		return time.Time{}, cleanStartupFailure(err)
	}
	second, err := cleanStartupMarker(cfg.idReader)
	if err != nil || first == second {
		return time.Time{}, cleanStartupFailure(ErrConfiguration)
	}
	if err := postCleanStartupBarrier(scanCtx, cfg, ari, first); err != nil {
		return time.Time{}, cleanStartupFailure(err)
	}
	firstTimestamp, err := readThroughCleanStartupMarker(scanCtx, cfg, pbx, ari, first)
	if err != nil {
		return time.Time{}, cleanStartupFailure(err)
	}
	if err := inspectCleanStartupChannels(scanCtx, cfg, ari); err != nil {
		return time.Time{}, cleanStartupFailure(err)
	}
	if err := postCleanStartupBarrier(scanCtx, cfg, ari, second); err != nil {
		return time.Time{}, cleanStartupFailure(err)
	}
	cutoff, err := readThroughCleanStartupMarker(scanCtx, cfg, pbx, ari, second)
	if err != nil {
		return time.Time{}, cleanStartupFailure(err)
	}
	if cutoff.Before(firstTimestamp) {
		return time.Time{}, cleanStartupFailure(ErrProtocol)
	}
	// This info request, both markers, the channel enumeration and all events
	// share one WebSocket. A lost socket is terminal; there is no HTTP fallback
	// that could silently attach this adapter to a replacement PBX.
	if _, err := ari.inspectPBX(scanCtx, pbx.Version, pbx.EntityID, pbx.StartupTime); err != nil {
		return time.Time{}, cleanStartupFailure(err)
	}
	if err := contextErr(scanCtx); err != nil {
		return time.Time{}, cleanStartupFailure(err)
	}
	return cutoff, nil
}

func cleanStartupMarker(reader io.Reader) (string, error) {
	random := make([]byte, 24)
	if _, err := io.ReadFull(reader, random); err != nil {
		return "", ErrConfiguration
	}
	return cleanStartupMarkerPrefix + base64.RawURLEncoding.EncodeToString(random), nil
}

func postCleanStartupBarrier(
	ctx context.Context,
	cfg normalizedConfig,
	ari *ariWSClient,
	marker string,
) error {
	response, err := ari.do(ctx, http.MethodPost, []string{"events", "user", marker}, url.Values{
		"application": {cfg.application},
	})
	if err != nil {
		return err
	}
	if response.status != http.StatusNoContent || len(response.body) != 0 {
		if status2xx(response.status) {
			return ErrProtocol
		}
		return ErrTransport
	}
	return nil
}

func readThroughCleanStartupMarker(
	ctx context.Context,
	cfg normalizedConfig,
	pbx pbxIdentity,
	ari *ariWSClient,
	marker string,
) (time.Time, error) {
	for count := 0; count < maxCleanStartupEvents; count++ {
		if err := contextErr(ctx); err != nil {
			return time.Time{}, err
		}
		event, err := ari.nextEvent(ctx)
		if err != nil {
			return time.Time{}, err
		}
		if !validOpaque(event.Type, 1, 128) || event.Application != cfg.application ||
			!validOpaque(event.AsteriskID, 1, 256) || event.AsteriskID != pbx.EntityID {
			return time.Time{}, ErrProtocol
		}
		if event.Type == "ChannelUserevent" && event.EventName == marker {
			if event.Channel != nil || event.Caller != nil || event.Peer != nil || len(event.Args) != 0 || event.DialStatus != "" {
				return time.Time{}, ErrProtocol
			}
			timestamp, ok := parseAsteriskTime(event.Timestamp)
			if !ok {
				return time.Time{}, ErrProtocol
			}
			return timestamp, nil
		}
		if strings.HasPrefix(event.EventName, cleanStartupMarkerPrefix) {
			return time.Time{}, sipgateway.ErrRecoveryRequired
		}
		if err := classifyCleanStartupEvent(cfg, event); err != nil {
			return time.Time{}, err
		}
	}
	return time.Time{}, ErrProtocol
}

func inspectCleanStartupChannels(
	ctx context.Context,
	cfg normalizedConfig,
	ari *ariWSClient,
) error {
	response, err := ari.do(ctx, http.MethodGet, []string{"channels"}, nil)
	if err != nil {
		return err
	}
	if response.status != http.StatusOK {
		return ErrTransport
	}
	var channels []ariChannel
	if err := decodeOneJSON(response.body, &channels); err != nil || len(channels) > maxCleanStartupChannels {
		return ErrProtocol
	}
	seen := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		if !validOpaque(channel.ID, 1, 256) || !validOpaque(channel.Name, 1, 512) ||
			!validOpaque(channel.State, 1, 64) {
			return ErrProtocol
		}
		if _, duplicate := seen[channel.ID]; duplicate {
			return ErrProtocol
		}
		seen[channel.ID] = struct{}{}
		switch classifyStartupChannel(cfg, channel) {
		case startupChannelUnrelated:
			continue
		case startupChannelOwned, startupChannelAmbiguous:
			return sipgateway.ErrRecoveryRequired
		default:
			return ErrProtocol
		}
	}
	return nil
}

func classifyCleanStartupEvent(cfg normalizedConfig, event ariEvent) error {
	switch event.Type {
	case "ApplicationReplaced", "ApplicationUnregistered":
		return sipgateway.ErrRecoveryRequired
	}

	knownChannelEvent := false
	switch event.Type {
	case "StasisStart", "StasisEnd", "ChannelCreated", "ChannelDestroyed", "ChannelStateChange",
		"ChannelDialplan", "ChannelHangupRequest", "Dial":
		knownChannelEvent = true
	}
	if event.Type == "StasisStart" {
		if len(event.Args) == 1 && event.Args[0] == cfg.incomingArgument {
			if event.Channel == nil {
				return ErrProtocol
			}
			if classifyStartupChannel(cfg, *event.Channel) == startupChannelUnrelated {
				return sipgateway.ErrRecoveryRequired
			}
		} else if slicesContainsString(event.Args, cfg.incomingArgument) {
			return ErrProtocol
		}
	}
	channels := []*ariChannel{event.Channel, event.Caller, event.Peer}
	foundChannel := false
	for _, channel := range channels {
		if channel == nil {
			continue
		}
		foundChannel = true
		if !validOpaque(channel.ID, 1, 256) || !validOpaque(channel.Name, 1, 512) ||
			!validOpaque(channel.State, 1, 64) {
			return ErrProtocol
		}
		switch classifyStartupChannel(cfg, *channel) {
		case startupChannelUnrelated:
		case startupChannelOwned, startupChannelAmbiguous:
			return sipgateway.ErrRecoveryRequired
		default:
			return ErrProtocol
		}
	}
	if knownChannelEvent && !foundChannel {
		return ErrProtocol
	}
	return nil
}

func classifyStartupChannel(cfg normalizedConfig, channel ariChannel) startupChannelClass {
	prefix := "PJSIP/" + cfg.incomingEndpoint + "-"
	endpointSuffix := strings.TrimPrefix(channel.Name, prefix)
	exactEndpoint := strings.HasPrefix(channel.Name, prefix) && validAsteriskChannelSuffix(endpointSuffix)
	exactIdentity := exactEndpoint && channel.Dialplan.Context == cfg.incomingContext &&
		channel.Dialplan.AppName == "Stasis" &&
		channel.Dialplan.AppData == cfg.application+","+cfg.incomingArgument &&
		channel.ChannelVars[incomingPolicyVariable] == cfg.incomingPolicyID
	if exactIdentity {
		return startupChannelOwned
	}
	// A partial match cannot be proven unrelated. In particular, a provider
	// channel may exist immediately before it enters Stasis, while a channel in
	// this exact app/argument may have a damaged or unexpected endpoint name.
	if strings.HasPrefix(channel.Name, prefix) ||
		channel.Dialplan.AppData == cfg.application+","+cfg.incomingArgument {
		return startupChannelAmbiguous
	}
	return startupChannelUnrelated
}

func slicesContainsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func cleanStartupFailure(err error) error {
	if err == nil {
		return sipgateway.ErrRecoveryRequired
	}
	if errors.Is(err, sipgateway.ErrRecoveryRequired) {
		return err
	}
	return errors.Join(sipgateway.ErrRecoveryRequired, err)
}
