// Package bus is the MQTT message bus every service talks over.
//
// Topics (all JSON):
//
//	homeapp/rooms              retained  []model.Room      (one publisher per source)
//	homeapp/devices/<id>       retained  model.Device      (empty payload = removed)
//	homeapp/commands/<id>                model.Command
//	homeapp/events/<name>                free-form events (automations, notifier)
//	homeapp/hints/zigbee-rooms retained  map[ieee]room id (HA → zigbee bridge)
package bus

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"hearth/internal/model"
)

const (
	Prefix         = "homeapp"
	TopicRooms     = Prefix + "/rooms/"    // + source
	TopicDevices   = Prefix + "/devices/"  // + device id
	TopicCommands  = Prefix + "/commands/" // + device id
	TopicEventsAll = Prefix + "/events/#"
	// TopicZigbeeRooms (retained) maps Zigbee IEEE address → room id. The HA bridge
	// publishes it while Home Assistant still owns room assignments.
	TopicZigbeeRooms = Prefix + "/hints/zigbee-rooms"
)

type Bus struct {
	c mqtt.Client
}

// Connect opens a connection that reconnects automatically and re-subscribes
// on reconnect.
func Connect(url, clientID, user, pass string, onConnect func(*Bus)) (*Bus, error) {
	b := &Bus{}
	opts := mqtt.NewClientOptions().
		AddBroker(url).
		SetClientID(clientID).
		SetUsername(user).
		SetPassword(pass).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(3 * time.Second).
		SetOrderMatters(false).
		SetOnConnectHandler(func(mqtt.Client) {
			slog.Info("bus connected", "broker", url)
			if onConnect != nil {
				onConnect(b)
			}
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			slog.Warn("bus connection lost", "err", err)
		})
	b.c = mqtt.NewClient(opts)
	tok := b.c.Connect()
	if !tok.WaitTimeout(30 * time.Second) {
		b.c.Disconnect(0)
		return nil, fmt.Errorf("mqtt connect %s: no answer in 30s (wrong address, or login refused)", url)
	}
	if err := tok.Error(); err != nil {
		return nil, fmt.Errorf("mqtt connect %s: %w", url, err)
	}
	return b, nil
}

func (b *Bus) publish(topic string, retained bool, v any) error {
	var payload []byte
	if v != nil {
		var err error
		if payload, err = json.Marshal(v); err != nil {
			return err
		}
	}
	tok := b.c.Publish(topic, 1, retained, payload)
	tok.WaitTimeout(10 * time.Second)
	return tok.Error()
}

func (b *Bus) PublishRooms(source string, rooms []model.Room) error {
	return b.publish(TopicRooms+source, true, rooms)
}

func (b *Bus) PublishDevice(d model.Device) error {
	return b.publish(TopicDevices+d.ID, true, d)
}

// RemoveDevice clears the retained message so the device disappears.
func (b *Bus) RemoveDevice(id string) error {
	tok := b.c.Publish(TopicDevices+id, 1, true, []byte{})
	tok.WaitTimeout(10 * time.Second)
	return tok.Error()
}

func (b *Bus) SendCommand(cmd model.Command) error {
	return b.publish(TopicCommands+cmd.DeviceID, false, cmd)
}

func (b *Bus) PublishEvent(name string, v any) error {
	return b.publish(Prefix+"/events/"+name, false, v)
}

func (b *Bus) subscribe(topic string, h func(topic string, payload []byte)) error {
	tok := b.c.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) {
		h(m.Topic(), m.Payload())
	})
	tok.WaitTimeout(10 * time.Second)
	return tok.Error()
}

// OnRooms delivers every source's room list.
func (b *Bus) OnRooms(h func(source string, rooms []model.Room)) error {
	return b.subscribe(TopicRooms+"+", func(t string, p []byte) {
		var rooms []model.Room
		if len(p) > 0 && json.Unmarshal(p, &rooms) != nil {
			return
		}
		h(strings.TrimPrefix(t, TopicRooms), rooms)
	})
}

// OnDevice delivers device updates; dev is nil when the device was removed.
func (b *Bus) OnDevice(h func(id string, dev *model.Device)) error {
	return b.subscribe(TopicDevices+"+", func(t string, p []byte) {
		id := strings.TrimPrefix(t, TopicDevices)
		if len(p) == 0 {
			h(id, nil)
			return
		}
		var d model.Device
		if err := json.Unmarshal(p, &d); err != nil {
			slog.Warn("bad device payload", "id", id, "err", err)
			return
		}
		h(id, &d)
	})
}

// OnCommand delivers commands for devices whose id starts with prefix
// (each bridge only handles its own devices).
func (b *Bus) OnCommand(prefix string, h func(cmd model.Command)) error {
	// "#" must be a whole topic level, so subscribe to all commands and filter.
	return b.subscribe(TopicCommands+"+", func(t string, p []byte) {
		if !strings.HasPrefix(strings.TrimPrefix(t, TopicCommands), prefix) {
			return
		}
		var c model.Command
		if err := json.Unmarshal(p, &c); err != nil {
			slog.Warn("bad command payload", "err", err)
			return
		}
		h(c)
	})
}

// Subscribe gives raw topic access to bridges whose devices already speak
// MQTT themselves (Zigbee2MQTT shares this broker).
func (b *Bus) Subscribe(topic string, h func(topic string, payload []byte)) error {
	return b.subscribe(topic, h)
}

// PublishRaw publishes a JSON value to an arbitrary topic (not retained).
func (b *Bus) PublishRaw(topic string, v any) error {
	return b.publish(topic, false, v)
}

// PublishRetained publishes a JSON value as the retained state of a topic.
func (b *Bus) PublishRetained(topic string, v any) error {
	return b.publish(topic, true, v)
}

// OnEvent delivers events (command_failed, …) published by any service.
func (b *Bus) OnEvent(h func(name string, data json.RawMessage)) error {
	return b.subscribe(TopicEventsAll, func(t string, p []byte) {
		h(strings.TrimPrefix(t, Prefix+"/events/"), json.RawMessage(p))
	})
}
