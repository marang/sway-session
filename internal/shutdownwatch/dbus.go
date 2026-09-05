package shutdownwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const (
	loginDestination = "org.freedesktop.login1"
	managerInterface = "org.freedesktop.login1.Manager"
	sessionInterface = "org.freedesktop.login1.Session"
	propertiesIface  = "org.freedesktop.DBus.Properties"
	busInterface     = "org.freedesktop.DBus"
)

var (
	managerPath   = dbus.ObjectPath("/org/freedesktop/login1")
	busPath       = dbus.ObjectPath("/org/freedesktop/DBus")
	sessionsPath  = dbus.ObjectPath("/org/freedesktop/login1/session")
	logindMatches = [][]dbus.MatchOption{
		{
			dbus.WithMatchSender(busInterface),
			dbus.WithMatchObjectPath(busPath),
			dbus.WithMatchInterface(busInterface),
			dbus.WithMatchMember("NameOwnerChanged"),
			dbus.WithMatchArg(0, loginDestination),
		},
		{
			dbus.WithMatchSender(loginDestination),
			dbus.WithMatchObjectPath(managerPath),
			dbus.WithMatchInterface(managerInterface),
			dbus.WithMatchMember("PrepareForShutdown"),
		},
		{
			dbus.WithMatchSender(loginDestination),
			dbus.WithMatchObjectPath(managerPath),
			dbus.WithMatchInterface(managerInterface),
			dbus.WithMatchMember("PrepareForSleep"),
		},
		{
			dbus.WithMatchSender(loginDestination),
			dbus.WithMatchObjectPath(managerPath),
			dbus.WithMatchInterface(managerInterface),
			dbus.WithMatchMember("SessionRemoved"),
		},
		{
			dbus.WithMatchSender(loginDestination),
			dbus.WithMatchPathNamespace(sessionsPath),
			dbus.WithMatchInterface(propertiesIface),
			dbus.WithMatchMember("PropertiesChanged"),
		},
	}
)

type connectResult struct {
	client loginClient
	err    error
}

func connectSystemBus(runCtx, setupCtx context.Context) (loginClient, error) {
	handler := &inlineSignalHandler{}
	result := make(chan connectResult)
	go func() {
		connection, err := dbus.ConnectSystemBus(
			dbus.WithContext(runCtx),
			dbus.WithSignalHandler(handler),
		)
		connected := connectResult{err: err}
		if err == nil {
			connected.client = &dbusClient{connection: connection, handler: handler}
		}
		select {
		case result <- connected:
		case <-runCtx.Done():
			if connection != nil {
				_ = connection.Close()
			}
		}
	}()

	select {
	case connected := <-result:
		return connected.client, connected.err
	case <-setupCtx.Done():
		return nil, setupCtx.Err()
	}
}

type dbusClient struct {
	connection *dbus.Conn
	handler    *inlineSignalHandler
}

func (client *dbusClient) Subscribe(ctx context.Context, sink func(event)) error {
	if sink == nil {
		return errors.New("nil lifecycle signal sink")
	}
	client.handler.setSink(sink)
	for _, match := range logindMatches {
		if err := client.connection.AddMatchSignalContext(ctx, match...); err != nil {
			return err
		}
	}
	return nil
}

func (client *dbusClient) NameOwner(ctx context.Context) (string, error) {
	var owner string
	err := client.connection.BusObject().CallWithContext(
		ctx,
		busInterface+".GetNameOwner",
		0,
		loginDestination,
	).Store(&owner)
	return owner, err
}

func (client *dbusClient) Inhibit(ctx context.Context) (io.Closer, error) {
	if !client.connection.SupportsUnixFDs() {
		return nil, errors.New("system D-Bus connection did not negotiate Unix file descriptor passing")
	}
	var descriptor dbus.UnixFD
	err := client.connection.Object(loginDestination, managerPath).CallWithContext(
		ctx,
		managerInterface+".Inhibit",
		0,
		"shutdown:sleep",
		"sway-session",
		"Finish persistent session state work",
		"delay",
	).Store(&descriptor)
	if err != nil {
		return nil, err
	}
	if descriptor < 0 {
		return nil, fmt.Errorf("logind returned invalid inhibitor file descriptor %d", descriptor)
	}
	unix.CloseOnExec(int(descriptor))
	file := os.NewFile(uintptr(descriptor), "logind-inhibitor")
	if file == nil {
		return nil, errors.New("logind returned unusable inhibitor file descriptor")
	}
	return file, nil
}

func (client *dbusClient) SessionByPID(ctx context.Context, pid uint32) (string, error) {
	var path dbus.ObjectPath
	err := client.connection.Object(loginDestination, managerPath).CallWithContext(
		ctx,
		managerInterface+".GetSessionByPID",
		0,
		pid,
	).Store(&path)
	if err != nil {
		return "", err
	}
	if !path.IsValid() {
		return "", fmt.Errorf("logind returned invalid session object path %q", path)
	}
	return string(path), nil
}

func (client *dbusClient) SessionState(ctx context.Context, path string) (string, error) {
	if !dbus.ObjectPath(path).IsValid() {
		return "", fmt.Errorf("invalid session object path %q", path)
	}
	value, err := client.property(ctx, dbus.ObjectPath(path), sessionInterface, "State")
	if err != nil {
		return "", err
	}
	state, ok := value.Value().(string)
	if !ok {
		return "", fmt.Errorf("state property has type %T, want string", value.Value())
	}
	return state, nil
}

func (client *dbusClient) PreparingForShutdown(ctx context.Context) (bool, error) {
	return client.managerBooleanProperty(ctx, "PreparingForShutdown")
}

func (client *dbusClient) PreparingForSleep(ctx context.Context) (bool, error) {
	return client.managerBooleanProperty(ctx, "PreparingForSleep")
}

func (client *dbusClient) managerBooleanProperty(ctx context.Context, name string) (bool, error) {
	value, err := client.property(ctx, managerPath, managerInterface, name)
	if err != nil {
		return false, err
	}
	result, ok := value.Value().(bool)
	if !ok {
		return false, fmt.Errorf("%s property has type %T, want bool", name, value.Value())
	}
	return result, nil
}

func (client *dbusClient) property(
	ctx context.Context,
	path dbus.ObjectPath,
	interfaceName string,
	propertyName string,
) (dbus.Variant, error) {
	var value dbus.Variant
	err := client.connection.Object(loginDestination, path).CallWithContext(
		ctx,
		propertiesIface+".Get",
		0,
		interfaceName,
		propertyName,
	).Store(&value)
	return value, err
}

func (client *dbusClient) Done() <-chan struct{} {
	return client.connection.Context().Done()
}

func (client *dbusClient) Close() error {
	return client.connection.Close()
}

type inlineSignalHandler struct {
	mu   sync.RWMutex
	sink func(event)
}

func (handler *inlineSignalHandler) setSink(sink func(event)) {
	handler.mu.Lock()
	handler.sink = sink
	handler.mu.Unlock()
}

// DeliverSignal deliberately invokes the monitor inline on godbus's reader
// goroutine. This ensures the guard is unsafe before the delay inhibitor is
// released, even if the daemon's ordinary event loop is busy.
func (handler *inlineSignalHandler) DeliverSignal(iface, member string, signal *dbus.Signal) {
	handler.mu.RLock()
	sink := handler.sink
	handler.mu.RUnlock()
	if sink == nil || signal == nil {
		return
	}

	received, relevant := decodeSignal(iface, member, signal)
	if relevant {
		sink(received)
	}
}

func decodeSignal(iface, member string, signal *dbus.Signal) (event, bool) {
	switch iface + "." + member {
	case managerInterface + ".PrepareForShutdown":
		if signal.Path != managerPath {
			return malformedSignal(signal, "PrepareForShutdown used an unexpected object path"), true
		}
		active, err := signalBoolean(signal)
		return event{kind: eventPrepareShutdown, source: signal.Sender, active: active, err: err}, true
	case managerInterface + ".PrepareForSleep":
		if signal.Path != managerPath {
			return malformedSignal(signal, "PrepareForSleep used an unexpected object path"), true
		}
		active, err := signalBoolean(signal)
		return event{kind: eventPrepareSleep, source: signal.Sender, active: active, err: err}, true
	case managerInterface + ".SessionRemoved":
		if signal.Path != managerPath || len(signal.Body) != 2 {
			return malformedSignal(signal, "SessionRemoved had an invalid body or object path"), true
		}
		path, pathOK := signal.Body[1].(dbus.ObjectPath)
		if _, idOK := signal.Body[0].(string); !idOK || !pathOK || !path.IsValid() {
			return malformedSignal(signal, "SessionRemoved had invalid arguments"), true
		}
		return event{kind: eventSessionRemoved, source: signal.Sender, path: string(path)}, true
	case propertiesIface + ".PropertiesChanged":
		if !pathInNamespace(signal.Path, sessionsPath) {
			return event{}, false
		}
		return decodePropertiesChanged(signal)
	case busInterface + ".NameOwnerChanged":
		if signal.Path != busPath || signal.Sender != busInterface || len(signal.Body) != 3 {
			return malformedSignal(signal, "NameOwnerChanged had an invalid sender, body, or object path"), true
		}
		name, nameOK := signal.Body[0].(string)
		_, oldOK := signal.Body[1].(string)
		_, newOK := signal.Body[2].(string)
		if !nameOK || !oldOK || !newOK || name != loginDestination {
			return malformedSignal(signal, "NameOwnerChanged had invalid arguments"), true
		}
		return event{kind: eventNameOwnerChanged, source: signal.Sender}, true
	default:
		return event{}, false
	}
}

func decodePropertiesChanged(signal *dbus.Signal) (event, bool) {
	if len(signal.Body) != 3 {
		return malformedSignal(signal, "PropertiesChanged had an invalid body"), true
	}
	interfaceName, interfaceOK := signal.Body[0].(string)
	changed, changedOK := signal.Body[1].(map[string]dbus.Variant)
	invalidated, invalidatedOK := signal.Body[2].([]string)
	if !interfaceOK || !changedOK || !invalidatedOK {
		return malformedSignal(signal, "PropertiesChanged had invalid arguments"), true
	}
	if interfaceName != sessionInterface {
		return event{}, false
	}

	if value, ok := changed["State"]; ok {
		state, ok := value.Value().(string)
		if !ok {
			return malformedSignal(signal, "session State change was not a string"), true
		}
		return event{kind: eventSessionState, source: signal.Sender, path: string(signal.Path), state: state}, true
	}
	for _, property := range invalidated {
		if property == "State" {
			return event{
				kind:        eventSessionState,
				source:      signal.Sender,
				path:        string(signal.Path),
				invalidated: true,
			}, true
		}
	}
	return event{}, false
}

func signalBoolean(signal *dbus.Signal) (bool, error) {
	if len(signal.Body) != 1 {
		return false, errors.New("preparation signal had an invalid body")
	}
	active, ok := signal.Body[0].(bool)
	if !ok {
		return false, fmt.Errorf("preparation signal argument has type %T, want bool", signal.Body[0])
	}
	return active, nil
}

func malformedSignal(signal *dbus.Signal, message string) event {
	return event{kind: eventInvalid, source: signal.Sender, path: string(signal.Path), err: errors.New(message)}
}

func pathInNamespace(path, namespace dbus.ObjectPath) bool {
	prefix := string(namespace) + "/"
	value := string(path)
	return value == string(namespace) || len(value) > len(prefix) && value[:len(prefix)] == prefix
}
