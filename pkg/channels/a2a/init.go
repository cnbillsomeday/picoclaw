// PicoClaw - Ultra-lightweight personal AI agent

package a2a

import (
	"fmt"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

func init() {
	channels.RegisterFactory(
		config.ChannelA2A,
		func(channelName, channelType string, cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
			bc := cfg.Channels[channelName]
			if bc == nil {
				return nil, fmt.Errorf("a2a: channel %q is not configured", channelName)
			}
			decoded, err := bc.GetDecoded()
			if err != nil {
				return nil, err
			}
			settings, ok := decoded.(*config.A2ASettings)
			if !ok {
				return nil, fmt.Errorf("a2a: channel %q has invalid settings type %T", channelName, decoded)
			}
			ch, err := NewA2AChannel(bc, settings, b)
			if err != nil {
				return nil, err
			}
			if channelName != config.ChannelA2A {
				ch.SetName(channelName)
			}
			return ch, nil
		},
	)
}
