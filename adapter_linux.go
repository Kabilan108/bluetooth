//go:build !baremetal

// Some documentation for the BlueZ D-Bus interface:
// https://git.kernel.org/pub/scm/bluetooth/bluez.git/tree/doc

package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

const defaultAdapter = "hci0"

type Adapter struct {
	id                   string
	scanCancelChan       chan struct{}
	bus                  *dbus.Conn
	bluez                dbus.BusObject // object at /
	adapter              dbus.BusObject // object at /org/bluez/hciX
	address              string
	defaultAdvertisement *Advertisement

	connectHandler func(device Device, connected bool)

	watcherCancel context.CancelFunc

	deviceStates    map[dbus.ObjectPath]bool // track connection state
	deviceStatesMtx sync.Mutex
}

// NewAdapter creates a new Adapter with the given ID.
//
// Make sure to call Enable() before using it to initialize the adapter.
func NewAdapter(id string) *Adapter {
	return &Adapter{
		id:             id,
		connectHandler: func(device Device, connected bool) {},
		deviceStates:   make(map[dbus.ObjectPath]bool),
	}
}

// DefaultAdapter is the default adapter on the system. On Linux, it is the
// first adapter available.
//
// Make sure to call Enable() before using it to initialize the adapter.
var DefaultAdapter = NewAdapter(defaultAdapter)

// Enable configures the BLE stack. It must be called before any
// Bluetooth-related calls (unless otherwise indicated).
func (a *Adapter) Enable() (err error) {
	bus, err := dbus.SystemBus()
	if err != nil {
		return err
	}
	a.bus = bus
	a.bluez = a.bus.Object("org.bluez", dbus.ObjectPath("/"))
	a.adapter = a.bus.Object("org.bluez", dbus.ObjectPath("/org/bluez/"+a.id))
	addr, err := a.adapter.GetProperty("org.bluez.Adapter1.Address")
	if err != nil {
		if err, ok := err.(dbus.Error); ok && err.Name == "org.freedesktop.DBus.Error.UnknownObject" {
			return fmt.Errorf("bluetooth: adapter %s does not exist", a.adapter.Path())
		}
		return fmt.Errorf("could not activate BlueZ adapter: %w", err)
	}
	addr.Store(&a.address)

	// pre-seed device connection states
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if call := a.bluez.Call("org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0); call.Err == nil {
		if err := call.Store(&objects); err == nil {
			adapterPrefix := "/org/bluez/" + a.id + "/dev_"
			for path, ifaces := range objects {
				if !strings.HasPrefix(string(path), adapterPrefix) {
					continue
				}
				if devProps, ok := ifaces["org.bluez.Device1"]; ok {
					if connVar, ok := devProps["Connected"]; ok {
						if connected, ok := connVar.Value().(bool); ok && connected {
							a.deviceStates[path] = true
						}
					}
				}
			}
		}
	}

	// start connection watcher
	ctx, cancel := context.WithCancel(context.Background())
	a.watcherCancel = cancel
	go a.watchDeviceProperties(ctx)

	return nil
}

func (a *Adapter) watchDeviceProperties(ctx context.Context) {
	signalCh := make(chan *dbus.Signal)
	a.bus.Signal(signalCh)
	defer a.bus.RemoveSignal(signalCh)

	// match rule for property changes on org.bluez.Device1 interfaces
	matchOpts := []dbus.MatchOption{
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchPathNamespace(dbus.ObjectPath("/org/bluez/" + a.id)),
	}
	if err := a.bus.AddMatchSignal(matchOpts...); err != nil {
		return // cannot recover - stop silently
	}
	defer a.bus.RemoveMatchSignal(matchOpts...)

	adapterPathPrefix := string(a.adapter.Path()) + "/dev_"

	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-signalCh:
			// Ensure it's a PropertiesChanged signal for a Device1 interface
			if len(sig.Body) < 2 {
				continue
			}
			ifaceName, ok := sig.Body[0].(string)
			if !ok || ifaceName != "org.bluez.Device1" {
				continue
			}

			changedProps, ok := sig.Body[1].(map[string]dbus.Variant)
			if !ok {
				continue
			}

			// check if the 'Connected' property changed
			if connectedVariant, propChanged := changedProps["Connected"]; propChanged {
				isConnected, _ := connectedVariant.Value().(bool)

				if debug {
					println("'Connected' property changed:", isConnected)
				}

				devPath := sig.Path

				a.deviceStatesMtx.Lock()
				wasConnected := a.deviceStates[devPath] // defaults to false if not present

				switch {
				case wasConnected && !isConnected:
					// disconnect detected
					a.deviceStates[devPath] = false
					a.deviceStatesMtx.Unlock()

					// construct device object
					macString := strings.ReplaceAll(
						strings.TrimPrefix(string(devPath), adapterPathPrefix), "_", ":",
					)
					mac, err := ParseMAC(macString)
					if err != nil {
						continue
					}
					device := Device{
						Address: Address{MACAddress{MAC: mac}},
						device:  a.bus.Object("org.bluez", devPath),
						adapter: a,
					}
					if a.connectHandler != nil {
						if debug {
							println("calling connectHandler")
						}
						a.connectHandler(device, false)
					}
				case !wasConnected && isConnected:
					// connect detected
					a.deviceStates[devPath] = true
					a.deviceStatesMtx.Unlock()
				default:
					// no state change
					a.deviceStatesMtx.Unlock()
				}
			}
		}
	}
}

func (a *Adapter) Disable() {
	// cancel the watcher
	if a.watcherCancel != nil {
		a.watcherCancel()
		if debug {
			println("cleaned up device property watcher")
		}
	}
}

func (a *Adapter) Address() (MACAddress, error) {
	if a.address == "" {
		return MACAddress{}, errors.New("adapter not enabled")
	}
	mac, err := ParseMAC(a.address)
	if err != nil {
		return MACAddress{}, err
	}
	return MACAddress{MAC: mac}, nil
}
