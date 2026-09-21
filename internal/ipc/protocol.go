// Package ipc defines the newline-delimited JSON protocol used between the
// IntraFlow host process (tray + orchestrator) and the GUI process (Wails
// webview). The host runs an IPC server on a unix socket; the GUI is a client
// that calls host methods by name and receives structured responses.
//
// # Protocol
//
// A request is a single JSON object on one line (terminated by '\n'):
//
//	{"id":1,"method":"ListDomains","params":{}}
//
// The response is a single JSON object on one line:
//
//	{"id":1,"result":[...]}            // success
//	{"id":1,"error":{"message":"..."}} // failure
//
// `id` is an arbitrary caller-chosen integer echoed back so the client can
// match responses to requests (useful if pipelining). `method` is one of the
// RPC method names listed below. `params` is an object whose shape depends on
// the method; methods with no parameters use {}.
//
// The server closes the connection on EOF. The client may open one persistent
// connection for its whole lifetime (requests are multiplexed by id).
//
// # Model (OpenSpec split-domain-forward-model)
//
// Domains and forwards are independent lists. The frontend calls ListDomains
// and ListForwards separately; SaveAll applies both lists in one two-phase
// hosts-write. Pause/Resume are global state toggles that also touch the hosts
// zone and therefore follow the same two-phase Prepare/Complete pattern.
package ipc

import "encoding/json"

// Request is a single RPC call from client to host.
type Request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Response is the host's reply. Exactly one of Result or Error is set.
type Response struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorBody      `json:"error,omitempty"`
}

// ErrorBody carries a failure detail. The frontend treats error.message as
// the user-facing string.
type ErrorBody struct {
	Message string `json:"message"`
}

// Event is a server→client push message. Unlike a Response, it has no client
// request id and instead carries an `event` name and an opaque `data` payload
// (a JSON RawMessage so the client can decode it lazily per event type). The
// server writes events unsolicited whenever backend state changes; the client
// dispatches them to a registered handler (see Client.OnEvent).
//
// On the wire an event looks like:
//
//	{"event":"recordsChanged","data":{}}
//
// (id is absent). The client distinguishes events from responses by probing
// the incoming line for a non-empty `event` field — see Client.readLoop.
type Event struct {
	Name string          `json:"event"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Event names broadcast by the host to connected GUIs.
const (
	// EventRecordsChanged is broadcast whenever a domain/forward is added,
	// modified, deleted, paused, resumed, or fixed (from IPC handlers or
	// tray callbacks). The GUI re-fetches its lists on receipt. The payload
	// is empty; the frontend refetches via ListDomains/ListForwards.
	EventRecordsChanged = "recordsChanged"

	// EventOpenSettings is broadcast when the user clicks "设置..." in the
	// tray while the GUI is already open; the GUI emits a Wails "open-settings"
	// event so the frontend opens the settings modal.
	EventOpenSettings = "openSettings"

	// EventFocusWindow is broadcast when the user clicks "打开主界面" in the
	// tray while the GUI is already open. The host cannot manipulate the GUI
	// process's window directly (separate process), so it asks the running GUI
	// to raise and focus its window: the GUI emits a Wails "focusWindow" event
	// and the frontend calls WindowUnminimise + WindowShow. When no GUI is
	// running, the host instead spawns one (the normal window shows itself).
	// The payload is empty.
	EventFocusWindow = "focusWindow"

	// EventElevatePause is broadcast when the user clicks "暂停托管" in the
	// tray while the GUI is already open. The host cannot run osascript (it
	// is a background menu-bar daemon), so it asks the running GUI to perform
	// the elevated hosts write by calling its own Pause() facade method,
	// which runs the two-phase PreparePause → WriteHostsElevated →
	// CompletePause flow. When no GUI is open, the host instead spawns a
	// short-lived GUI process with --elevate=pause to run that same flow.
	EventElevatePause = "elevatePause"

	// EventElevateResume is the resume counterpart of EventElevatePause.
	EventElevateResume = "elevateResume"

	// EventConfirmQuit is broadcast when the user clicks "退出" in the tray
	// while hosting is active (not paused and at least one domain or forward
	// is configured). The host does NOT quit immediately — the GUI re-emits
	// a Wails "confirmQuit" event so the frontend can show a confirmation
	// modal, and the user's choice drives a subsequent QuitHost IPC call
	// (tear down) or no-op (cancel). When no GUI is running, the host
	// instead spawns a short-lived GUI process with --confirm-quit to run
	// the same confirmation flow. The payload is empty.
	EventConfirmQuit = "confirmQuit"
)

// Method names. These mirror the App facade methods the GUI calls; the
// two-phase hosts-write methods (Prepare*/Complete*) are internal to the
// GUI-side orchestration and are not bound to the frontend directly.
const (
	// Read methods.
	MethodListDomains = "ListDomains"
	MethodListForwards = "ListForwards"

	// Batch apply (SaveAll facade on the GUI side orchestrates the two
	// phases). PrepareApplyAll validates + builds hosts content (no
	// elevation); CompleteApplyAll reconciles config + forwarders.
	MethodPrepareApplyAll  = "PrepareApplyAll"
	MethodCompleteApplyAll = "CompleteApplyAll"

	// Pause / Resume (GUI-side facade orchestrates the two phases).
	MethodPreparePause  = "PreparePause"
	MethodCompletePause = "CompletePause"
	MethodPrepareResume  = "PrepareResume"
	MethodCompleteResume = "CompleteResume"

	// FixNow (GUI-side facade orchestrates the two phases).
	MethodPrepareFixNow  = "PrepareFixNow"
	MethodCompleteFixNow = "CompleteFixNow"

	// Settings + autostart.
	MethodGetSettings = "GetSettings"
	MethodSetSettings = "SetSettings"
	// MethodSetAutoStart toggles the OS-native login-item / LaunchAgent /
	// Run-key registration. Unlike SetSettings (which persists app prefs),
	// this operates directly on the OS autostart registration and reports
	// success/failure synchronously. The live autostart state is surfaced
	// read-only via SettingsDTO.AutoStartState on GetSettings.
	MethodSetAutoStart = "setAutoStart"

	// Diagnostics / hosts zone preview / port helpers.
	MethodGetStartupDiff     = "GetStartupDiff"
	MethodCurrentHostsZone   = "CurrentHostsZone"
	MethodCheckPortAvailable = "CheckPortAvailable"
	MethodSuggestFreePort    = "SuggestFreePort"
	MethodIsPrivilegedPort   = "IsPrivilegedPort"

	// MethodQuitHost tears down the host process (kills any running GUI
	// child and quits the systray loop). It is invoked by the frontend's
	// quit-confirmation modal when the user confirms quitting while hosting
	// is active. The host's tray onQuit handler gates the direct teardown
	// path on (paused OR no domains AND no forwards); the active-hosting
	// path instead routes through EventConfirmQuit / --confirm-quit and
	// ultimately calls this method on user confirmation. Returns
	// SimpleResult{OK:true}.
	MethodQuitHost = "QuitHost"
)

// Param shapes for request marshaling. Each is the params object for the
// corresponding method.

// SetSettingsParams is the params object for MethodSetSettings. AutoStartState
// is read-only and ignored by the host.
type SetSettingsParams struct {
	Settings SettingsDTO `json:"settings"`
}

// SetAutoStartParams is the params object for MethodSetAutoStart. Enabled
// true registers IntraFlow to launch at login; false unregisters it.
type SetAutoStartParams struct {
	Enabled bool `json:"enabled"`
}

type CheckPortAvailableParams struct {
	Port int `json:"port"`
}

type SuggestFreePortParams struct {
	From int `json:"from"`
}

type IsPrivilegedPortParams struct {
	Port int `json:"port"`
}

// SaveAllParams carries the full desired domain + forward sets to
// PrepareApplyAll / CompleteApplyAll. Entries with empty IDs are treated as
// new and are assigned fresh IDs by the host (returned in
// PrepareApplyAllResult.AssignedDomains / AssignedForwards).
type SaveAllParams struct {
	Domains  []DomainDTO  `json:"domains"`
	Forwards []ForwardDTO `json:"forwards"`
}

// SaveAllPayload is the frontend-facing SaveAll method's single parameter
// shape. It is identical to SaveAllParams but given a distinct name so the
// Wails binding generator emits a stable JS signature
// `SaveAll({domains, forwards})`. The frontend calls
// `SaveAll({ domains: [...], forwards: [...] })`.
type SaveAllPayload struct {
	Domains  []DomainDTO  `json:"domains"`
	Forwards []ForwardDTO `json:"forwards"`
}

// PrepareApplyAllResult is returned by PrepareApplyAll. On success it carries
// the new hosts content (which the GUI writes elevated only when Changed is
// true), whether it differs from the current hosts file, and the domains and
// forwards with IDs assigned. The GUI passes the assigned sets verbatim to
// CompleteApplyAll. On failure the error envelope is populated and
// HostsContent is empty.
type PrepareApplyAllResult struct {
	// HostsContent is the full new /etc/hosts content the GUI should write
	// via osascript/pkexec. Empty when an error occurred.
	HostsContent string `json:"hostsContent,omitempty"`
	// Changed is true when HostsContent differs from the current hosts
	// file. When false, the GUI should skip the elevated write entirely
	// (no password prompt) and proceed straight to CompleteApplyAll.
	Changed bool `json:"changed"`
	// AssignedDomains / AssignedForwards are the assigned sets (new entries
	// have a fresh ID). The GUI passes these verbatim to CompleteApplyAll.
	// Empty when an error occurred.
	AssignedDomains  []DomainDTO  `json:"assignedDomains,omitempty"`
	AssignedForwards []ForwardDTO `json:"assignedForwards,omitempty"`
	// Error envelope (mirrors SaveResultDTO's error branches). Validation
	// errors from config.ValidateDomains/ValidateForwards are surfaced here
	// as ValidationError; all other failures as OtherError.
	ValidationError string `json:"validationError,omitempty"`
	OtherError      string `json:"otherError,omitempty"`
}

// PrepareResult is returned by PreparePause / PrepareResume / PrepareFixNow.
// It carries the new hosts file content the GUI must write elevated (when
// Changed is true) and an error envelope on failure.
type PrepareResult struct {
	// HostsContent is the full new /etc/hosts content the GUI should write
	// via osascript/pkexec. Empty when an error occurred.
	HostsContent string `json:"hostsContent,omitempty"`
	// Changed is true when HostsContent differs from the current hosts
	// file. When false, the GUI should skip the elevated write entirely
	// (no password prompt) and proceed straight to the Complete call.
	Changed bool `json:"changed"`
	// Error envelope.
	OtherError string `json:"otherError,omitempty"`
}

// --- DTOs ---
// These are the wire shapes the frontend already expects (Lane D / des-1
// contract). The JSON field names are camelCase and MUST NOT drift.

// DomainDTO mirrors config.Domain for the frontend.
type DomainDTO struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
}

// ForwardDTO mirrors config.Forward for the frontend.
type ForwardDTO struct {
	ID         string `json:"id"`
	ListenPort int    `json:"listenPort"`
	TargetHost string `json:"targetHost"`
	TargetPort int    `json:"targetPort"`
	Enabled    bool   `json:"enabled"`
}

// StatusInfoDTO describes a forwarder's state for the list view. State is one
// of "listening", "stopped", "error" (forwarder.Status.String()).
type StatusInfoDTO struct {
	State      string `json:"state"`
	Reason     string `json:"reason"`
	ResolvedIP string `json:"resolvedIP"`
}

// ForwardStatusDTO pairs a forward with its forwarder status. The frontend
// reads item.forward and item.status.
type ForwardStatusDTO struct {
	Forward ForwardDTO     `json:"forward"`
	Status  StatusInfoDTO  `json:"status"`
}

// SettingsDTO mirrors config.Settings for the frontend, augmented with the
// live OS autostart state.
//
// AutoStartState is READ-ONLY: it is populated host-side from
// runtime.GetAutoStartState() and reflects the actual OS login-item
// registration (not a persisted preference). SetSettings ignores any value
// the client sends for it; toggling autostart goes through SetAutoStart.
//
// Values: "enabled", "disabled", "requires_approval" (the latter means the
// registration exists but macOS requires the user to approve it in System
// Settings → General → Login Items). Empty is treated as "disabled" by the
// frontend.
type SettingsDTO struct {
	Paused            bool   `json:"paused"`
	DNSRefreshMinutes int    `json:"dnsRefreshMinutes"`
	AutoStartState    string `json:"autoStartState"`
}

// DiffDTO mirrors hosts.Diff for the frontend banner.
type DiffDTO struct {
	MissingInHosts  []string `json:"missingInHosts"`
	MissingInConfig []string `json:"missingInConfig"`
	Consistent      bool     `json:"consistent"`
}

// SaveResultDTO is returned by SaveAll. On success SavedDomains / SavedForwards
// carry the assigned sets (with IDs); the frontend refetches via
// ListDomains/ListForwards regardless. The error fields carry any failure:
// authCancelled (user dismissed the elevation prompt), validationError (a
// config.ValidateDomains/ValidateForwards failure, matched by message
// substring), or otherError (anything else).
type SaveResultDTO struct {
	SavedDomains      []DomainDTO  `json:"savedDomains"`
	SavedForwards     []ForwardDTO `json:"savedForwards"`
	AuthCancelled     bool         `json:"authCancelled"`
	ValidationError   string       `json:"validationError,omitempty"`
	OtherError        string       `json:"otherError,omitempty"`
}

// SimpleResult is a generic ok/error envelope for void operations
// (Pause/Resume/FixNow/SetSettings/SetAutoStart).
type SimpleResult struct {
	OK            bool   `json:"ok"`
	AuthCancelled bool   `json:"authCancelled"`
	Error         string `json:"error,omitempty"`
}