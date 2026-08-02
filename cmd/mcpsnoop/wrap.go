package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
)

// wrapClient is one MCP client whose config wrap and unwrap can edit.
//
// Adding a second client is meant to be a new file, never an edit to this one:
// write a file that calls registerWrapClient from init with the client's name,
// the well-known path to its config, and the object key its servers live under.
// wrap_claude_desktop.go is the template.
type wrapClient struct {
	name        string                 // the --client value
	display     string                 // how the client is named in output
	serversKey  string                 // the config object holding one entry per server
	configPath  func() (string, error) // the well-known config location
	restartHint string                 // what the user has to do for the edit to take effect
}

var wrapClients = map[string]wrapClient{}

// registerWrapClient adds a client to the registry. It is called from init, so a
// name collision is a build-time mistake rather than a runtime surprise, and
// panicking is the only way to say so before main starts.
func registerWrapClient(c wrapClient) {
	if _, dup := wrapClients[c.name]; dup {
		panic("mcpsnoop: duplicate wrap client " + c.name)
	}
	wrapClients[c.name] = c
}

func lookupWrapClient(name string) (wrapClient, error) {
	c, ok := wrapClients[name]
	if !ok {
		return wrapClient{}, badInput("unknown client %q; known clients: %s",
			name, strings.Join(slices.Sorted(maps.Keys(wrapClients)), ", "))
	}
	return c, nil
}

// backupSuffix names the copy wrap takes of the untouched config. It sits next
// to the config rather than under the mcpsnoop state directory so it is
// discoverable by anyone looking at the file they are worried about, and so it
// survives an MCPSNOOP_HOME change.
const backupSuffix = ".mcpsnoop.bak"

// wrapFault is an error that carries the exit code it should produce: 2 when the
// user can fix it by typing something else (unknown client, unknown server, a
// server that is not stdio), 1 when it is file state (missing, unreadable or
// malformed config, a failed write). That split is the one prune and check use.
type wrapFault struct {
	code int
	err  error
}

func (f wrapFault) Error() string { return f.err.Error() }
func (f wrapFault) Unwrap() error { return f.err }

func badInput(format string, a ...any) error { return wrapFault{2, fmt.Errorf(format, a...)} }
func badState(format string, a ...any) error { return wrapFault{1, fmt.Errorf(format, a...)} }

func reportFault(cmd *cobra.Command, verb string, err error) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "mcpsnoop %s: %v\n", verb, err)
	var fault wrapFault
	if errors.As(err, &fault) {
		return exitCode(fault.code)
	}
	return exitCode(1)
}

// wrapperPath is indirected so tests get a deterministic command instead of the
// test binary, the same seam convention runShimFn and runHTTPFn use.
var wrapperPath = mcpsnoopPath

// mcpsnoopPath is the command wrap writes into the config. It is the absolute
// path of the running binary, not the bare "mcpsnoop" the README shows, because
// a desktop client is a GUI app: it spawns servers with the launchd or Explorer
// default PATH, which holds neither ~/go/bin nor /opt/homebrew/bin, so a bare
// name often will not resolve there even though it resolves in your shell.
func mcpsnoopPath() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "mcpsnoop"
	}
	return exe
}

func newWrapCmd() *cobra.Command {
	var clientName, configPath string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "wrap <server>",
		Short: "Route one of a client's MCP servers through mcpsnoop",
		Long: "Edit an MCP client's config so the named server starts through mcpsnoop, which is the one manual step between installing mcpsnoop and seeing traffic.\n\n" +
			"Only that server's entry is rewritten. The config is copied to <config>" + backupSuffix + " first, every other byte of the file is left exactly as it was, and mcpsnoop unwrap puts the entry back. Running it twice is a no-op, and --dry-run shows the change without writing anything.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runWrap(cmd, clientName, configPath, args[0], dryRun); err != nil {
				return reportFault(cmd, "wrap", err)
			}
			return nil
		},
	}
	addWrapFlags(cmd, &clientName, &configPath, &dryRun)
	return cmd
}

func newUnwrapCmd() *cobra.Command {
	var clientName, configPath string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "unwrap <server>",
		Short: "Take mcpsnoop back out of a client's MCP server entry",
		Long: "Undo mcpsnoop wrap for one server, so the client launches it directly again.\n\n" +
			"When the rest of the config is still as wrap left it, the file is restored byte for byte from <config>" + backupSuffix + " and the backup is removed. When it has changed since, only the named server's entry is rewritten so those changes survive, and the backup is kept. Running it on a server that is not wrapped is a no-op, and --dry-run writes nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runUnwrap(cmd, clientName, configPath, args[0], dryRun); err != nil {
				return reportFault(cmd, "unwrap", err)
			}
			return nil
		},
	}
	addWrapFlags(cmd, &clientName, &configPath, &dryRun)
	return cmd
}

func addWrapFlags(cmd *cobra.Command, clientName, configPath *string, dryRun *bool) {
	flags := cmd.Flags()
	flags.SortFlags = false
	flags.StringVar(clientName, "client", claudeDesktopClient, "MCP client whose config to edit")
	flags.StringVar(configPath, "config", "", "path to the client config, defaults to its well-known location")
	flags.BoolVar(dryRun, "dry-run", false, "show the change without writing anything")
}

// wrapTarget is a located server entry plus everything needed to write the file
// back.
type wrapTarget struct {
	client wrapClient
	path   string
	config []byte      // the config file, verbatim
	mode   fs.FileMode // its current permissions, preserved across the rewrite
	member jsonMember  // where the server's entry sits in config
	entry  serverEntry // that entry, parsed
}

func (t wrapTarget) backupPath() string { return t.path + backupSuffix }

// resolveTarget locates one server's entry in a client config. Both commands
// start here, so a missing config, a malformed one, and an unknown server report
// identically whichever way you came in.
func resolveTarget(clientName, configPath, server string) (wrapTarget, error) {
	client, err := lookupWrapClient(clientName)
	if err != nil {
		return wrapTarget{}, err
	}
	path := configPath
	if path == "" {
		if path, err = client.configPath(); err != nil {
			return wrapTarget{}, badState("%w", err)
		}
	}

	config, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return wrapTarget{}, badState("no %s config at %s; add the server there first, or pass --config with the path to it", client.display, path)
	case err != nil:
		return wrapTarget{}, badState("cannot read %s: %w", path, err)
	}

	// Preserve the config's own permissions. A stat failure here is not worth
	// aborting for, so fall back to owner-only, which is the safe direction: an
	// mcpServers entry routinely carries API keys in its env block.
	mode := fs.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	member, err := findServerMember(config, client.serversKey, server, path)
	if err != nil {
		return wrapTarget{}, err
	}
	entry, err := parseServerEntry(member.value, server)
	if err != nil {
		return wrapTarget{}, err
	}
	return wrapTarget{client: client, path: path, config: config, mode: mode, member: member, entry: entry}, nil
}

func runWrap(cmd *cobra.Command, clientName, configPath, server string, dryRun bool) error {
	t, err := resolveTarget(clientName, configPath, server)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	if isWrapped(t.entry.command) {
		// Nothing is written, which is what makes a second wrap idempotent, and it
		// is also what keeps the backup holding the pre-wrap config rather than a
		// wrapped one.
		fmt.Fprintf(out, "%q is already wrapped in %s, nothing to do\n", server, t.client.display)
		return nil
	}
	if t.entry.command == "" {
		return badInput("%q is not a stdio server, so there is no command to wrap; mcpsnoop proxies a streamable-HTTP server with `mcpsnoop http --target <url>` instead", server)
	}

	// The wrapped entry keeps every other key the user wrote, and everything the
	// server used to be launched with moves behind mcpsnoop's own "--".
	command := wrapperPath()
	args := append([]string{"--", t.entry.command}, t.entry.args...)
	members := maps.Clone(t.entry.members)
	if err := setMember(members, "command", command); err != nil {
		return err
	}
	if err := setMember(members, "args", args); err != nil {
		return err
	}
	rewritten, err := spliceMember(t.config, t.member, members)
	if err != nil {
		return err
	}

	before, after := commandLine(t.entry.command, t.entry.args), commandLine(command, args)
	if dryRun {
		fmt.Fprintln(out, "dry run, nothing was written")
		printChange(out, t.path, before, after)
		return nil
	}

	// The backup is written and closed before the config is touched, so a backup
	// that cannot be written aborts with the config still untouched.
	if err := writeFileAtomic(t.backupPath(), t.config, 0o600); err != nil {
		return badState("cannot write the backup %s: %w", t.backupPath(), err)
	}
	if err := writeFileAtomic(t.path, rewritten, t.mode); err != nil {
		return badState("cannot write %s: %w", t.path, err)
	}

	fmt.Fprintf(out, "wrapped %q in %s\n", server, t.client.display)
	printChange(out, t.path, before, after)
	fmt.Fprintf(out, "  backup:  %s\n", t.backupPath())
	fmt.Fprintf(out, "%s, then run mcpsnoop to watch the traffic\n", t.client.restartHint)
	return nil
}

func runUnwrap(cmd *cobra.Command, clientName, configPath, server string, dryRun bool) error {
	t, err := resolveTarget(clientName, configPath, server)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	if !isWrapped(t.entry.command) {
		fmt.Fprintf(out, "%q is not wrapped in %s, nothing to do\n", server, t.client.display)
		return nil
	}
	command, args, err := unwrappedCommand(t.entry.args, server)
	if err != nil {
		return err
	}

	members := maps.Clone(t.entry.members)
	if err := setMember(members, "command", command); err != nil {
		return err
	}
	if len(args) > 0 {
		if err := setMember(members, "args", args); err != nil {
			return err
		}
	} else {
		// A server with no arguments has no args key. Leaving an empty list behind
		// would be a change to the entry that wrap never made.
		delete(members, "args")
	}
	rewritten, err := spliceMember(t.config, t.member, members)
	if err != nil {
		return err
	}

	// Prefer a literal byte-for-byte restore, but only when this run can prove the
	// backup describes the same config: if the user edited the file while it was
	// wrapped, restoring the backup would silently throw those edits away, so the
	// spliced result is written instead and the backup is kept.
	restored, backup := false, t.backupPath()
	if original, err := os.ReadFile(backup); err == nil && sameJSON(original, rewritten) {
		rewritten, restored = original, true
	}

	before, after := commandLine(t.entry.command, t.entry.args), commandLine(command, args)
	if dryRun {
		fmt.Fprintln(out, "dry run, nothing was written")
		printChange(out, t.path, before, after)
		printRestoreNote(out, backup, restored, dryRun)
		return nil
	}

	if err := writeFileAtomic(t.path, rewritten, t.mode); err != nil {
		return badState("cannot write %s: %w", t.path, err)
	}
	if restored {
		// Only now, with the original back in place, is the backup redundant.
		if err := os.Remove(backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return badState("unwrapped %s, but cannot remove the backup %s: %w", t.path, backup, err)
		}
	}

	fmt.Fprintf(out, "unwrapped %q in %s\n", server, t.client.display)
	printChange(out, t.path, before, after)
	printRestoreNote(out, backup, restored, dryRun)
	fmt.Fprintf(out, "%s\n", t.client.restartHint)
	return nil
}

func printChange(out io.Writer, path, before, after string) {
	fmt.Fprintf(out, "  config:  %s\n", path)
	fmt.Fprintf(out, "  before:  %s\n", before)
	fmt.Fprintf(out, "  after:   %s\n", after)
}

// printRestoreNote says which of unwrap's two endings happened, or would have:
// the whole file put back from the backup, or just this entry rewritten because
// the rest of the config had moved on since it was wrapped.
func printRestoreNote(out io.Writer, backup string, restored, dryRun bool) {
	switch {
	case restored && dryRun:
		fmt.Fprintf(out, "  would restore the config byte for byte from %s, and remove it\n", backup)
	case restored:
		fmt.Fprintf(out, "  restored the config byte for byte from %s, and removed it\n", backup)
	default:
		if _, err := os.Stat(backup); err != nil {
			return // never wrapped by this mcpsnoop, so there is nothing to say
		}
		fmt.Fprintf(out, "  the rest of the config changed since it was wrapped, so only this entry is rewritten and the original stays at %s\n", backup)
	}
}

func commandLine(command string, args []string) string {
	return strings.Join(append([]string{command}, args...), " ")
}

// serverEntry is one server's config entry. The members are kept raw so every
// key mcpsnoop does not model, env, type, or a client-specific extension,
// survives a rewrite exactly as the user wrote it.
type serverEntry struct {
	members map[string]json.RawMessage
	command string
	args    []string
}

func parseServerEntry(raw json.RawMessage, server string) (serverEntry, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return serverEntry{}, badState("the entry for %q is not a JSON object: %w", server, err)
	}
	entry := serverEntry{members: members}
	if v, ok := members["command"]; ok {
		if err := json.Unmarshal(v, &entry.command); err != nil {
			return serverEntry{}, badState("the %q entry's \"command\" is not a string", server)
		}
	}
	if v, ok := members["args"]; ok {
		if err := json.Unmarshal(v, &entry.args); err != nil {
			return serverEntry{}, badState("the %q entry's \"args\" is not a list of strings", server)
		}
	}
	return entry, nil
}

// isWrapped reports whether a command already runs through mcpsnoop.
//
// filepath.Base is deliberately not used. It does not split on a backslash off
// Windows, so a Windows config inspected on any other OS, which is exactly what
// a test does, would look unwrapped and get wrapped a second time. labelFor
// splits the same way for the same reason.
func isWrapped(command string) bool {
	name := command
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if base, ok := strings.CutSuffix(strings.ToLower(name), ".exe"); ok {
		name = base
	}
	return strings.EqualFold(name, "mcpsnoop")
}

// unwrappedCommand recovers the command a wrapped entry originally ran. Anything
// before the first "--" is mcpsnoop's own flags, so an entry a user wrote by hand
// as `mcpsnoop --redact-secrets -- npx x` unwraps to `npx x` and loses only the
// flags, which is the whole point of unwrapping.
func unwrappedCommand(args []string, server string) (string, []string, error) {
	i := slices.Index(args, "--")
	if i < 0 {
		return "", nil, badState("the %q entry runs mcpsnoop but its args hold no \"--\", so the server command it wrapped cannot be recovered; edit the entry by hand", server)
	}
	rest := args[i+1:]
	if len(rest) == 0 {
		return "", nil, badState("the %q entry runs mcpsnoop with nothing after \"--\", so there is no server command to restore; edit the entry by hand", server)
	}
	return rest[0], rest[1:], nil
}

// setMember encodes one value into an entry, through jsonwire rather than
// encoding/json. encoding/json escapes &, < and > by default, so an argument
// like --url=https://host/path?a=1&b=2 would land in the user's config with its
// & written as \u0026, changing the argument their server is launched with.
func setMember(members map[string]json.RawMessage, key string, value any) error {
	raw, err := jsonwire.Marshal(value)
	if err != nil {
		return badState("cannot encode %q: %w", key, err)
	}
	members[key] = raw
	return nil
}

// jsonMember is one member of a JSON object together with the exact byte range
// its value occupies in the enclosing document.
type jsonMember struct {
	name  string
	start int
	end   int
	value json.RawMessage
}

// objectMembers returns every member of the JSON object obj with its byte span.
//
// Decoding into a json.RawMessage hands back the value's original bytes,
// internal whitespace included, and InputOffset is the offset just past them, so
// end-len(value) is the value's exact start. That is what lets wrap rewrite one
// server's entry and leave the user's key order, indentation and every other
// server in the file untouched, which reformatting the whole document through
// Unmarshal and MarshalIndent could not do.
func objectMembers(obj []byte) ([]jsonMember, error) {
	dec := json.NewDecoder(bytes.NewReader(obj))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("expected a JSON object")
	}
	var members []jsonMember
	for dec.More() {
		nameTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := nameTok.(string)
		if !ok {
			return nil, errors.New("expected a JSON object")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		end := int(dec.InputOffset())
		start := end - len(value)
		if start < 0 || end > len(obj) || !bytes.Equal(obj[start:end], value) {
			// Belt and braces. The span drives a write over somebody's config, so it
			// is checked against the source rather than trusted.
			return nil, fmt.Errorf("cannot locate the bytes of %q", name)
		}
		members = append(members, jsonMember{name: name, start: start, end: end, value: value})
	}
	return members, nil
}

func findServerMember(config []byte, serversKey, server, path string) (jsonMember, error) {
	top, err := objectMembers(config)
	if err != nil {
		return jsonMember{}, badState("cannot read %s as a JSON object: %w", path, err)
	}
	i := slices.IndexFunc(top, func(m jsonMember) bool { return m.name == serversKey })
	if i < 0 {
		return jsonMember{}, badState("%s has no %q section, so it configures no MCP servers", path, serversKey)
	}
	section := top[i]

	entries, err := objectMembers(section.value)
	if err != nil {
		return jsonMember{}, badState("the %q section of %s is not a JSON object: %w", serversKey, path, err)
	}
	j := slices.IndexFunc(entries, func(m jsonMember) bool { return m.name == server })
	if j < 0 {
		names := make([]string, len(entries))
		for k, e := range entries {
			names[k] = e.name
		}
		slices.Sort(names)
		if len(names) == 0 {
			return jsonMember{}, badInput("no server named %q in %s; its %q section is empty", server, path, serversKey)
		}
		return jsonMember{}, badInput("no server named %q in %s; it configures %s", server, path, strings.Join(names, ", "))
	}

	// The spans came out of the section's own bytes, so shift them to the file.
	entry := entries[j]
	entry.start += section.start
	entry.end += section.start
	return entry, nil
}

// spliceMember rewrites just the bytes of member and returns the whole file,
// with every byte outside that range identical to what came in.
func spliceMember(config []byte, member jsonMember, members map[string]json.RawMessage) ([]byte, error) {
	block, err := encodeMembers(config, member, members)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(config)-(member.end-member.start)+len(block))
	out = append(out, config[:member.start]...)
	out = append(out, block...)
	out = append(out, config[member.end:]...)
	if !json.Valid(out) {
		return nil, badState("the rewritten config would not be valid JSON, so nothing was written")
	}
	return out, nil
}

// encodeMembers renders an entry the way the file around it is written: on one
// line if the entry it replaces was on one line, otherwise indented to continue
// from the entry's own line, with the file's own indent unit.
func encodeMembers(config []byte, member jsonMember, members map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := jsonwire.NewEncoder(&buf)
	if bytes.ContainsRune(member.value, '\n') {
		enc.SetIndent(lineIndent(config, member.start), indentUnit(config))
	}
	if err := enc.Encode(members); err != nil {
		return nil, badState("cannot encode the server entry: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// lineIndent is the leading whitespace of the line offset sits on, which is the
// indentation a spliced entry has to continue from.
func lineIndent(config []byte, offset int) string {
	line := config[bytes.LastIndexByte(config[:offset], '\n')+1 : offset]
	return string(line[:len(line)-len(bytes.TrimLeft(line, " \t"))])
}

// indentUnit guesses the file's own indentation from its first indented line, so
// a wrapped entry keeps looking like the rest of the config instead of switching
// a four-space file to two. Two spaces is the fallback, matching the README.
func indentUnit(config []byte) string {
	for raw := range bytes.Lines(config) {
		line := bytes.TrimRight(raw, "\r\n")
		indent := line[:len(line)-len(bytes.TrimLeft(line, " \t"))]
		if len(indent) > 0 && len(indent) < len(line) {
			return string(indent)
		}
	}
	return "  "
}

// sameJSON reports whether two configs describe the same document, ignoring
// formatting and key order. Unmarshalling into any and re-encoding sorts object
// keys, so the comparison survives the reordering a re-encoded entry causes.
func sameJSON(a, b []byte) bool {
	na, err := normalizeJSON(a)
	if err != nil {
		return false
	}
	nb, err := normalizeJSON(b)
	if err != nil {
		return false
	}
	return bytes.Equal(na, nb)
}

func normalizeJSON(data []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return jsonwire.Marshal(v)
}

// writeFileAtomic replaces path's contents in one step, so an interrupted write
// can never leave a client staring at half a config. The temp file is created in
// the config's own directory, which keeps the rename on a single filesystem.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mcpsnoop-wrap-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
