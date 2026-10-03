package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

const workspaceHelp = "Multi-repo workspaces: one mission over several repositories."

const workspaceInitHelp = "Create (or extend) a multi-repo workspace: a git repository holding each --repo as a\n" +
	"git submodule (\"member\"), committed as one workspace commit.\n\n" +
	"Point a mission's --workdir at it: the anchor and every checkpoint live in the workspace,\n" +
	"each member's changes are committed inside the member first, checks and witnesses run from\n" +
	"the workspace root (`cmd:sh -c 'cd svc && make test'`), and the reviewer sees each member's\n" +
	"own diff. Parallel waves (--max-parallel) do not run across members."

const repoHelp = "A member repository: URL or local path, optionally NAME=URL and @REF (e.g. svc=git@host:org/svc.git@v2); repeatable."

// parseMemberSpec is python's parse_member_spec: [NAME=]URL[@REF] -> (name, url, ref); the name
// defaults to the URL's last path component without .git.
func parseMemberSpec(spec string) (name, url, ref string, err error) {
	text := pyfmt.PyStrip(spec)
	name, rest, found := strings.Cut(text, "=")
	if !found || strings.ContainsAny(name, "/:") {
		name, rest = "", text
	}
	url = rest
	if at := strings.LastIndex(rest, "@"); at > 0 && !strings.ContainsAny(rest[at:], "/:") {
		url, ref = rest[:at], rest[at+1:]
	}
	url = pyfmt.PyStrip(url)
	if url == "" {
		return "", "", "", fmt.Errorf("--repo %s: missing a URL or path", contracts.PyRepr(spec))
	}
	if name == "" {
		name = strings.TrimRight(url, "/")
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if i := strings.LastIndex(name, ":"); i >= 0 {
			name = name[i+1:]
		}
		name = strings.TrimSuffix(name, ".git")
	}
	name = pyfmt.PyStrip(name)
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") || strings.HasPrefix(name, ".") {
		return "", "", "", fmt.Errorf("--repo %s: %s is not a usable member name", contracts.PyRepr(spec), contracts.PyRepr(name))
	}
	return name, url, pyfmt.PyStrip(ref), nil
}

// workspaceCmd is python's lha workspace (init).
func (c *cli) workspaceCmd(args []string) error {
	usage := fmt.Sprintf("Usage: lha workspace [OPTIONS] COMMAND [ARGS]...\n\n%s\n\nCommands:\n  %-6s %s\n", workspaceHelp, "init", workspaceInitHelp)
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(c.stdout, usage)
		if len(args) == 0 {
			return &exitError{code: 2} // typer's no_args_is_help
		}
		return nil
	}
	if args[0] != "init" {
		return &exitError{code: 2, message: "Usage: lha workspace [OPTIONS] COMMAND [ARGS]...\nTry 'lha workspace --help' for help.\n\n" +
			"Error: No such command " + pyQuote(args[0]) + "."}
	}
	fs := c.newFlags("workspace init", workspaceInitHelp)
	var repos repeated
	fs.Var(&repos, "repo", repoHelp)
	positional, err := c.parseWithArgs(fs, args[1:])
	if err != nil {
		if err == flag.ErrHelp {
			return err
		}
		return err
	}
	switch {
	case len(positional) == 0:
		return &exitError{code: 2, message: "Usage: lha workspace init [OPTIONS] DIRECTORY\nTry 'lha workspace init --help' for help.\n\nError: Missing argument 'DIRECTORY'."}
	case len(positional) > 1:
		return &exitError{code: 2, message: fmt.Sprintf("Error: Got unexpected extra argument (%s)", positional[1])}
	case len(repos) == 0:
		return &exitError{code: 2, message: "Usage: lha workspace init [OPTIONS] DIRECTORY\nTry 'lha workspace init --help' for help.\n\nError: Missing option '--repo'."}
	}
	directory := positional[0]
	type spec struct{ name, url, ref string }
	specs := []spec{}
	seen := map[string]int{}
	for _, r := range repos {
		name, url, ref, err := parseMemberSpec(r)
		if err != nil {
			return fail(2, "%s", err.Error())
		}
		specs = append(specs, spec{name, url, ref})
		seen[name]++
	}
	repeats := []string{}
	for name, n := range seen {
		if n > 1 {
			repeats = append(repeats, name)
		}
	}
	if len(repeats) > 0 {
		sortStrings(repeats)
		return fail(2, "--repo names repeat: %s", strings.Join(repeats, ", "))
	}
	if st, err := os.Stat(filepath.Join(directory, state.AnchorDir)); err == nil && st.IsDir() {
		return fail(2, "%s already anchors a mission; members are added before a mission starts", contracts.PyRepr(directory))
	}
	if err := state.InitRepo(c.ctx, directory); err != nil {
		return err
	}
	existing, err := state.MemberPaths(c.ctx, directory)
	if err != nil {
		return err
	}
	isMember := map[string]bool{}
	for _, m := range existing {
		isMember[m] = true
	}
	added := []string{}
	for _, s := range specs {
		if isMember[s.name] {
			fmt.Fprintf(c.stderr, "%s: already a member, kept\n", s.name)
			continue
		}
		if err := state.AddMember(c.ctx, directory, s.url, s.name, s.ref); err != nil {
			return fail(1, "cannot add member %s from %s: %s", contracts.PyRepr(s.name), contracts.PyRepr(s.url), err.Error())
		}
		sha, err := state.HeadSHA(c.ctx, filepath.Join(directory, s.name))
		if err != nil {
			return err
		}
		at := ""
		if s.ref != "" {
			at = "@" + s.ref
		}
		fmt.Fprintf(c.stdout, "%s <- %s%s (%s)\n", s.name, s.url, at, sha[:12])
		added = append(added, s.name)
	}
	if len(added) > 0 {
		if _, err := state.CommitAll(c.ctx, directory, "lha: workspace members "+strings.Join(added, ", ")); err != nil {
			return err
		}
	}
	members, err := state.MemberPaths(c.ctx, directory)
	if err != nil {
		return err
	}
	plural := "s"
	if len(members) == 1 {
		plural = ""
	}
	fmt.Fprintf(c.stdout, "workspace %s: %d member%s\n", directory, len(members), plural)
	return nil
}

// refuseParallelWavesOnAWorkspace is python's _refuse_parallel_waves_on_a_workspace: parallel
// waves run implementers in git worktrees of the workspace, which do not carry its members.
func (c *cli) refuseParallelWavesOnAWorkspace(workdir string, maxParallel int) error {
	if maxParallel < 2 {
		return nil
	}
	if st, err := os.Stat(workdir); err != nil || !st.IsDir() {
		return nil
	}
	members, err := state.MemberPaths(c.ctx, workdir)
	if err != nil || len(members) == 0 {
		return nil
	}
	return fail(2, "--max-parallel %d: %s is a multi-repo workspace (members: %s); parallel waves do not run across members, use --max-parallel 1",
		maxParallel, contracts.PyRepr(workdir), strings.Join(members, ", "))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
