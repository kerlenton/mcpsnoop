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

// backupSuffix names the copy wrap takes of the config before it wraps anything.
// It sits next to the config rather than under the mcpsnoop state directory so
// it is discoverable by anyone looking at the file they are worried about, and
// so it survives an MCPSNOOP_HOME change.
//
// There is one per config, and the first wrap is the one that writes it. A later
// wrap of a second server leaves it alone: overwriting would replace the
// untouched config with one mcpsnoop had already edited, and then unwrapping
// that second server would match it, restore it, and delete the only copy of the
// file as the user wrote it while the first server was still wrapped.
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

// writeConfigHook runs just before the config is re-read and written. It is the
// only way to test the recheck, since the window it guards is a few microseconds
// wide and a test that raced for it would be the flakiest thing in the suite.
// Nil outside tests.
var writeConfigHook func()

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
			"Only that server's entry is rewritten, and its keys come back in alphabetical order. Every other byte of the file is left exactly as it was. The first wrap copies the config to <config>" + backupSuffix + " and a later wrap of a second server keeps that copy, so it always holds the config as it was before mcpsnoop touched anything. mcpsnoop unwrap puts the entry back. Running it twice is a no-op, and --dry-run shows the change without writing anything.",
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
			"When the rest of the config is still as wrap left it, the file is restored byte for byte from <config>" + backupSuffix + ". When it has changed since, only the named server's entry is rewritten so those changes survive. The backup is removed once no server in the config runs through mcpsnoop any more, and kept otherwise, since it holds a copy of everything in the config including any secrets in env blocks. Running it on a server that is not wrapped is a no-op, and --dry-run writes nothing.",
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
func resolveTarget(clientName, configPath, server string, configGiven bool) (wrapTarget, error) {
	client, err := lookupWrapClient(clientName)
	if err != nil {
		return wrapTarget{}, err
	}
	path := configPath
	if path == "" {
		// An explicitly empty --config is refused rather than treated as absent.
		// A script written as `mcpsnoop wrap "$SRV" --config "$CFG"` with CFG unset
		// would otherwise edit the user's live Claude Desktop config and drop a
		// backup beside it, which is the one case where this writes to a file the
		// caller never named.
		if configGiven {
			return wrapTarget{}, badInput("--config was given an empty path; omit it to use the well-known location")
		}
		if path, err = client.configPath(); err != nil {
			return wrapTarget{}, badState("%w", err)
		}
	}

	config, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return wrapTarget{}, badState("no %s config at %s; add the server there first, or pass --config with the path to it", client.display, path)
	case err != nil:
		return wrapTarget{}, badState("cannot read %s as JSON: %w", path, err)
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
	t, err := resolveTarget(clientName, configPath, server, cmd.Flags().Changed("config"))
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	if isWrapped(t.entry.command) {
		// Nothing is written, which is what makes a second wrap idempotent, and it
		// is also what keeps the backup holding the pre-wrap config rather than a
		// wrapped one.
		fmt.Fprintf(out, "%q is already wrapped in %s, nothing to do\n", server, t.client.display)
		// Only the name is matched, not the path, so an entry can be wrapped around
		// an mcpsnoop that has since moved or been deleted. Saying nothing there
		// leaves the user running the one command that would fix it and being told
		// there is nothing to fix.
		if current := wrapperPath(); t.entry.command != current {
			fmt.Fprintf(out, "  it runs %s, not this binary at %s\n", t.entry.command, current)
			fmt.Fprintf(out, "  run mcpsnoop unwrap %q and wrap it again to point it here\n", server)
		}
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
	// that cannot be written aborts with the config still untouched. An existing
	// one is kept, since it is the earlier and therefore less-edited snapshot.
	if _, err := os.Stat(t.backupPath()); errors.Is(err, fs.ErrNotExist) {
		if err := writeFileAtomic(t.backupPath(), t.config, 0o600); err != nil {
			return badState("cannot write the backup %s: %w", t.backupPath(), err)
		}
	}
	if err := t.writeConfig(rewritten); err != nil {
		return err
	}

	fmt.Fprintf(out, "wrapped %q in %s\n", server, t.client.display)
	printChange(out, t.path, before, after)
	fmt.Fprintf(out, "  backup:  %s\n", t.backupPath())
	fmt.Fprintf(out, "%s, then run mcpsnoop to watch the traffic\n", t.client.restartHint)
	return nil
}

func runUnwrap(cmd *cobra.Command, clientName, configPath, server string, dryRun bool) error {
	t, err := resolveTarget(clientName, configPath, server, cmd.Flags().Changed("config"))
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
	restored, removed, backup := false, false, t.backupPath()
	if original, err := os.ReadFile(backup); err == nil && sameJSON(original, rewritten) {
		rewritten, restored = original, true
	}

	before, after := commandLine(t.entry.command, t.entry.args), commandLine(command, args)
	if dryRun {
		fmt.Fprintln(out, "dry run, nothing was written")
		printChange(out, t.path, before, after)
		printRestoreNote(out, backup, restored, restored && !anyWrapped(rewritten, t.client.serversKey), dryRun)
		return nil
	}

	if err := t.writeConfig(rewritten); err != nil {
		return err
	}
	// The backup goes only when nothing in the file is wrapped any more. Removing
	// it here because this one entry came back would take the copy of the
	// untouched config away while another server still runs through mcpsnoop.
	if restored && !anyWrapped(rewritten, t.client.serversKey) {
		if err := os.Remove(backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return badState("unwrapped %s, but cannot remove the backup %s: %w", t.path, backup, err)
		}
		removed = true
	}

	fmt.Fprintf(out, "unwrapped %q in %s\n", server, t.client.display)
	printChange(out, t.path, before, after)
	printRestoreNote(out, backup, restored, removed, dryRun)
	fmt.Fprintf(out, "%s\n", t.client.restartHint)
	return nil
}

// writeConfig writes the config back, refusing if the file moved since it was
// read. wrap and unwrap are a read, a rewrite and a write, and Claude Desktop
// rewrites this same file whenever a connector is toggled, so without this the
// whole of somebody else's edit disappears with both sides reporting success.
// It narrows the window rather than closing it, which a lock file would do at
// the cost of a stale lock nobody can explain.
func (t wrapTarget) writeConfig(rewritten []byte) error {
	if writeConfigHook != nil {
		writeConfigHook()
	}
	switch current, err := os.ReadFile(t.path); {
	case err != nil:
		return badState("cannot re-read %s before writing it: %w", t.path, err)
	case !bytes.Equal(current, t.config):
		return badState("%s changed while mcpsnoop was working on it, so nothing was written; run the command again", t.path)
	}
	if err := writeFileAtomic(t.path, rewritten, t.mode); err != nil {
		return badState("cannot write %s: %w", t.path, err)
	}
	return nil
}

// anyWrapped reports whether any server in the config still runs through
// mcpsnoop, which is what decides whether the backup is still needed.
func anyWrapped(config []byte, serversKey string) bool {
	top, err := objectMembers(config)
	if err != nil {
		return true // cannot tell, so keep the backup
	}
	i := slices.IndexFunc(top, func(m jsonMember) bool { return m.name == serversKey })
	if i < 0 {
		return false
	}
	entries, err := objectMembers(top[i].value)
	if err != nil {
		return true
	}
	for _, e := range entries {
		entry, err := parseServerEntry(e.value, e.name)
		if err != nil {
			return true
		}
		if isWrapped(entry.command) {
			return true
		}
	}
	return false
}

func printChange(out io.Writer, path, before, after string) {
	fmt.Fprintf(out, "  config:  %s\n", path)
	fmt.Fprintf(out, "  before:  %s\n", before)
	fmt.Fprintf(out, "  after:   %s\n", after)
}

// printRestoreNote says which of unwrap's endings happened, or would have: the
// whole file put back from the backup, or just this entry rewritten because the
// rest of the config had moved on since it was wrapped. Either way it says what
// became of the backup, because it holds a second copy of everything in the
// config, env blocks and their API keys included, and a user who is not told it
// is still there has no reason to go looking for it.
func printRestoreNote(out io.Writer, backup string, restored, removed, dryRun bool) {
	restore, remove := "restored", "removed"
	if dryRun {
		restore, remove = "would restore", "would remove"
	}
	switch {
	case restored && removed:
		fmt.Fprintf(out, "  %s the config byte for byte from %s, and %s it, since nothing is wrapped any more\n",
			restore, backup, remove)
		return
	case restored:
		fmt.Fprintf(out, "  %s the config byte for byte from %s\n", restore, backup)
	default:
		if _, err := os.Stat(backup); err != nil {
			return // never wrapped by this mcpsnoop, so there is nothing to say
		}
		fmt.Fprintf(out, "  the rest of the config changed since it was wrapped, so only this entry is rewritten\n")
	}
	fmt.Fprintf(out, "  %s stays at %s; it is a copy of the config, secrets in env blocks included, so delete it once you are happy\n",
		"the backup", backup)
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
	seen := map[string]bool{}
	for dec.More() {
		nameTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := nameTok.(string)
		if !ok {
			return nil, errors.New("expected a JSON object")
		}
		// A repeated name is refused rather than resolved. Every JSON parser takes
		// the last one and this walk finds the first, so editing here would rewrite
		// an entry the client never reads: wrap would report success, the traffic
		// would not change, and the user would have nothing to go on.
		if seen[name] {
			return nil, fmt.Errorf("the member %q appears twice, so it is ambiguous which one the client reads", name)
		}
		seen[name] = true
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
		return jsonMember{}, badState("cannot read %s as JSON: %w", path, err)
	}
	i := slices.IndexFunc(top, func(m jsonMember) bool { return m.name == serversKey })
	if i < 0 {
		return jsonMember{}, badState("%s has no %q section, so it configures no MCP servers", path, serversKey)
	}
	section := top[i]

	entries, err := objectMembers(section.value)
	if err != nil {
		return jsonMember{}, badState("cannot read the %q section of %s as JSON: %w", serversKey, path, err)
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
	indented := bytes.ContainsRune(member.value, '\n')
	if indented {
		enc.SetIndent(lineIndent(config, member.start), indentUnit(config))
	}
	if err := enc.Encode(members); err != nil {
		return nil, badState("cannot encode the server entry: %w", err)
	}
	block := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	// encoding/json ends every line with a bare \n, so a Windows config came back
	// with CRLF outside the rewritten entry and LF inside it. Mixed terminators in
	// one file are a whole-file diff the next time anything normalises it, on the
	// platform this command goes out of its way to support.
	if indented && bytes.Contains(config, []byte("\r\n")) {
		block = bytes.ReplaceAll(block, []byte("\n"), []byte("\r\n"))
	}
	return block, nil
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
// formatting and key order. Key order has to be ignored because re-encoding an
// entry sorts its keys, so a byte comparison would never match.
//
// The answer decides whether unwrap overwrites the whole file with the backup,
// so a false yes silently throws away whatever the user changed in between.
// normalizeJSON is written for that, not for convenience.
func sameJSON(a, b []byte) bool {
	na, err := normalizeJSON(a)
	if err != nil {
		return false
	}
	nb, err := normalizeJSON(b)
	if err != nil {
		return false
	}
	return na == nb
}

// normalizeJSON renders a document in a canonical form that keeps every
// distinction the file makes.
//
// json.Unmarshal into any cannot be used here. It lands every number in a
// float64, so a config carrying an account id of 9007199254740992 compares equal
// to the same config after the user corrects it to ...93, and unwrap then
// restores the backup over the correction and deletes the backup. It also keeps
// only the last of a repeated key, hiding a difference in the other copy.
// Decoding with UseNumber keeps the literal the file spelled, and a repeated
// name is an error rather than a silent collapse.
func normalizeJSON(data []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var buf bytes.Buffer
	if err := normalizeValue(dec, &buf); err != nil {
		return "", err
	}
	// Trailing bytes would mean two documents in one file, which is not a config.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("trailing content after the JSON document")
	}
	return buf.String(), nil
}

// normalizeValue writes one value canonically, sorting object keys so a
// re-encoded entry still matches, and rejecting a repeated name so two documents
// that differ only in the copy a parser discards are never called equal.
func normalizeValue(dec *json.Decoder, buf *bytes.Buffer) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return writeScalar(tok, buf)
	}
	switch delim {
	case '{':
		members := map[string]string{}
		for dec.More() {
			nameTok, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := nameTok.(string)
			if !ok {
				return errors.New("expected an object member name")
			}
			if _, dup := members[name]; dup {
				return fmt.Errorf("the object member %q appears twice", name)
			}
			var value bytes.Buffer
			if err := normalizeValue(dec, &value); err != nil {
				return err
			}
			members[name] = value.String()
		}
		buf.WriteByte('{')
		for i, name := range slices.Sorted(maps.Keys(members)) {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeScalar(name, buf); err != nil {
				return err
			}
			buf.WriteByte(':')
			buf.WriteString(members[name])
		}
		buf.WriteByte('}')
	case '[':
		buf.WriteByte('[')
		for i := 0; dec.More(); i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := normalizeValue(dec, buf); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		return fmt.Errorf("unexpected %q", delim)
	}
	// The closing delimiter, which json.Decoder hands back as its own token.
	_, err = dec.Token()
	return err
}

// writeScalar renders one scalar. A json.Number goes out as the literal the file
// spelled, which is the whole point of decoding with UseNumber.
func writeScalar(tok json.Token, buf *bytes.Buffer) error {
	if n, ok := tok.(json.Number); ok {
		buf.WriteString(n.String())
		return nil
	}
	raw, err := jsonwire.Marshal(tok)
	if err != nil {
		return err
	}
	buf.Write(raw)
	return nil
}

// writeFileAtomic replaces path's contents in one step, so an interrupted write
// can never leave a client staring at half a config. The temp file is created in
// the config's own directory, which keeps the rename on a single filesystem.
//
// A symlink is followed first. os.Rename does not follow the final component, so
// renaming onto a link replaces the link with a regular file, which is how a
// config kept under stow or chezmoi silently stops being the file the dotfile
// repository manages. Resolving first writes the file the user actually keeps.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
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
