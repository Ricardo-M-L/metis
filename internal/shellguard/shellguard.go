// Package shellguard rejects raw process-termination commands emitted by the
// model. Metis-owned background jobs must be stopped through BashKill(job_id),
// which can only address processes registered in the current jobs.Registry.
package shellguard

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const maxNestedShellDepth = 8

var (
	ErrProcessTermination = errors.New("raw process termination commands are disabled; use BashKill(job_id) for Metis-owned background jobs")
	ErrSystemMutation     = errors.New("destructive system mutation is disabled")
)

// Check parses command as Bash and rejects kill-family executables in command
// position, including pipelines, command substitutions, wrappers, xargs /
// parallel targets, and static shell -c payloads. Text such as
// `echo "kill -9"` or `rg "pkill"` remains ordinary data and is allowed.
func Check(command string) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	return check(command, 0)
}

func check(command string, depth int) error {
	if depth > maxNestedShellDepth {
		return fmt.Errorf("%w (nested shell command exceeds inspection depth)", ErrProcessTermination)
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		// Execution may use zsh, cmd.exe, or PowerShell even though the common
		// inspection grammar is Bash. If that grammar cannot account for the
		// complete command, allowing it would turn a syntax difference into a
		// process-termination bypass. Fail closed and direct the model to the
		// scoped BashKill tool instead.
		return blocked("shell syntax cannot be inspected safely")
	}
	var guardErr error
	syntax.Walk(file, func(node syntax.Node) bool {
		if guardErr != nil {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		guardErr = inspectCall(call, depth)
		return guardErr == nil
	})
	return guardErr
}

func inspectCall(call *syntax.CallExpr, depth int) error {
	words := make([]shellWord, 0, len(call.Args))
	for _, word := range call.Args {
		value, ok := staticWord(word)
		singleArg := wordStaysSingleArg(word)
		// `[` is the POSIX test command. A lone opening bracket is not a
		// syntactically valid glob and every supported shell therefore keeps it
		// as one literal argv entry. The general cardinality guard must stay
		// conservative for real command-position patterns such as `k[il]l`.
		if ok && value == "[" {
			singleArg = true
		}
		words = append(words, shellWord{
			value:     value,
			static:    ok,
			singleArg: singleArg,
		})
	}
	if len(words) == 0 {
		return nil
	}
	return inspectWords(words, depth)
}

type shellWord struct {
	value     string
	static    bool
	singleArg bool
}

func inspectWords(words []shellWord, depth int) error {
	if len(words) == 0 {
		return nil
	}
	if !words[0].static || !words[0].singleArg || strings.TrimSpace(words[0].value) == "" {
		return blocked("dynamic command position cannot be inspected safely")
	}
	cmd := commandBase(words[0].value)
	if isKillCommand(cmd) {
		return blocked("blocked " + cmd)
	}
	if reason, deny := destructiveSystemMutation(cmd, words[1:]); deny {
		return blockedSystem(reason)
	}

	switch cmd {
	case "command":
		if len(words) >= 2 && words[1].static && (words[1].value == "-v" || words[1].value == "-V") {
			return nil
		}
		target, err := commandTarget(words[1:], map[string]bool{"-p": false})
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "builtin":
		target, err := commandTarget(words[1:], nil)
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "exec":
		target, err := commandTarget(words[1:], map[string]bool{"-a": true, "-c": false, "-l": false})
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "env":
		return inspectEnv(words[1:], depth)
	case "sudo", "doas":
		target, err := commandTarget(words[1:], sudoOptions)
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "nohup":
		target, err := commandTarget(words[1:], map[string]bool{"--help": false, "--version": false})
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "nice":
		return inspectNice(words[1:], depth)
	case "timeout":
		return inspectTimeout(words[1:], depth)
	case "setsid":
		target, err := commandTarget(words[1:], map[string]bool{"-c": false, "--ctty": false, "-f": false, "--fork": false, "-w": false, "--wait": false})
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "xargs":
		return inspectXargs(words[1:], depth)
	case "parallel":
		target, err := commandTarget(words[1:], parallelOptions)
		if err != nil {
			return err
		}
		if len(target) == 0 {
			return nil
		}
		// GNU parallel commonly receives the entire target as one quoted word.
		if target[0].static && strings.ContainsAny(target[0].value, " \t;|&()") {
			return check(target[0].value, depth+1)
		}
		return inspectWords(target, depth)
	case "find":
		return inspectFind(words[1:], depth)
	case "busybox", "toybox":
		target, err := commandTarget(words[1:], map[string]bool{"--install": false, "--list": false, "--list-full": false, "--show": false})
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "eval":
		payload, err := joinStatic(words[1:])
		if err != nil {
			return err
		}
		return check(payload, depth+1)
	case "sh", "bash", "zsh", "dash", "ksh":
		payload, found, err := staticShellPayload(words[1:])
		if err != nil {
			return err
		}
		if found {
			return check(payload, depth+1)
		}
	case "cmd", "cmd.exe":
		return inspectCmd(words[1:], depth)
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return inspectPowerShell(words[1:], depth)
	}
	return nil
}

func blocked(reason string) error {
	return fmt.Errorf("%w (%s)", ErrProcessTermination, reason)
}

func blockedSystem(reason string) error {
	return fmt.Errorf("%w (%s)", ErrSystemMutation, reason)
}

// destructiveSystemMutation recognizes the narrow set of machine- or
// cluster-wide changes that must never turn into an approval prompt. The
// caller invokes it after parsing a real command position, so quoted prose is
// still data and compound commands are checked one call at a time by the AST
// walk. Wrappers (sudo/command/env/etc.) recurse through inspectWords.
func destructiveSystemMutation(cmd string, words []shellWord) (string, bool) {
	switch cmd {
	case "apt", "apt-get", "dnf", "yum", "apk", "zypper", "snap", "flatpak", "dpkg", "rpm":
		return destructivePackageMutation(cmd, words)
	case "pacman":
		return destructivePacmanMutation(words)
	case "useradd", "adduser", "userdel", "deluser", "usermod",
		"groupadd", "addgroup", "groupdel", "delgroup", "groupmod",
		"chpasswd", "gpasswd", "newusers", "vipw", "vigr":
		return "account database management via " + cmd, true
	case "passwd":
		return destructivePasswdMutation(words)
	case "chage":
		return destructiveChageMutation(words)
	case "dscl":
		return destructiveDSCLMutation(words)
	case "sysadminctl":
		return destructiveSysadminctlMutation(words)
	case "net":
		return destructiveNetAccountMutation(words)
	case "iptables", "ip6tables", "ebtables", "arptables":
		return destructiveIPTablesMutation(cmd, words)
	case "iptables-restore", "ip6tables-restore", "ebtables-restore", "arptables-restore":
		return "firewall ruleset replacement via " + cmd, true
	case "nft":
		return destructiveNFTMutation(words)
	case "ufw":
		return destructiveUFWMutation(words)
	case "firewall-cmd":
		return destructiveFirewallCmdMutation(words)
	case "pfctl":
		return destructivePFCTLMutation(words)
	case "systemctl":
		return destructiveSystemctlMutation(words)
	case "service":
		return destructiveServiceMutation(words)
	case "launchctl":
		return destructiveLaunchctlMutation(words)
	case "docker":
		return destructiveDockerMutation(words)
	case "kubectl", "oc":
		return destructiveKubectlMutation(words)
	case "crontab":
		return destructiveCrontabMutation(words)
	default:
		return "", false
	}
}

func staticValues(words []shellWord) ([]string, bool) {
	values := make([]string, 0, len(words))
	for _, word := range words {
		if !word.static {
			return nil, false
		}
		values = append(values, word.value)
	}
	return values, true
}

func lowerSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[strings.ToLower(value)] = struct{}{}
	}
	return out
}

func containsLower(set map[string]struct{}, value string) bool {
	_, ok := set[strings.ToLower(value)]
	return ok
}

// firstOperand skips global options and their known values, returning the
// command's first positional action. Unknown options are treated as flags; a
// dynamic option/action is deliberately reported as uninspectable.
func firstOperand(words []shellWord, valueOptions map[string]struct{}) (string, int, bool) {
	for i := 0; i < len(words); i++ {
		if !words[i].static || !words[i].singleArg {
			return "", -1, false
		}
		value := words[i].value
		if value == "--" {
			if i+1 < len(words) {
				if !words[i+1].static || !words[i+1].singleArg {
					return "", -1, false
				}
				return strings.ToLower(words[i+1].value), i + 1, true
			}
			return "", -1, true
		}
		if strings.HasPrefix(value, "-") && value != "-" {
			name, inline := optionName(value)
			if _, needsValue := valueOptions[strings.ToLower(name)]; needsValue && !inline {
				i++
				if i >= len(words) || !words[i].singleArg {
					return "", -1, false
				}
			}
			continue
		}
		return strings.ToLower(value), i, true
	}
	return "", -1, true
}

var packageOptionValues = lowerSet(
	"-o", "--option", "-c", "--config", "--config-file", "--root", "--installroot",
	"--releasever", "--repo", "--repository", "--from-repo", "--cache-dir", "--cachedir",
	"--target", "--admindir", "--instdir", "--dbpath", "--rcfile",
)

var packageMutationActions = map[string]map[string]struct{}{
	"apt":     lowerSet("install", "remove", "purge", "upgrade", "full-upgrade", "dist-upgrade", "autoremove", "update", "reinstall", "satisfy"),
	"apt-get": lowerSet("install", "remove", "purge", "upgrade", "full-upgrade", "dist-upgrade", "dselect-upgrade", "autoremove", "update", "reinstall", "satisfy"),
	"dnf":     lowerSet("install", "remove", "erase", "upgrade", "update", "downgrade", "reinstall", "swap", "distro-sync", "groupinstall", "groupremove", "groupupgrade", "autoremove", "module"),
	"yum":     lowerSet("install", "remove", "erase", "upgrade", "update", "downgrade", "reinstall", "swap", "distro-sync", "groupinstall", "groupremove", "groupupdate", "autoremove"),
	"apk":     lowerSet("add", "del", "upgrade", "fix"),
	"zypper":  lowerSet("install", "in", "remove", "rm", "update", "up", "dist-upgrade", "dup", "patch", "modifyrepo", "addrepo", "removerepo"),
	"snap":    lowerSet("install", "remove", "refresh", "revert", "enable", "disable", "connect", "disconnect", "alias", "unalias", "set", "unset"),
	"flatpak": lowerSet("install", "uninstall", "update", "repair", "mask", "pin", "remote-add", "remote-delete", "remote-modify", "override"),
	"dpkg":    lowerSet("--install", "-i", "--unpack", "--configure", "--remove", "-r", "--purge", "-p", "--update-avail", "--merge-avail", "--clear-avail"),
	"rpm":     lowerSet("--install", "-i", "--upgrade", "-u", "--freshen", "-f", "--erase", "-e", "--rebuilddb", "--initdb", "--setperms", "--setugids"),
}

func destructivePackageMutation(cmd string, words []shellWord) (string, bool) {
	if cmd == "dpkg" || cmd == "rpm" {
		values, static := staticValues(words)
		if !static {
			return "dynamic " + cmd + " action cannot be inspected safely", true
		}
		for _, value := range values {
			name, _ := optionName(value)
			if containsLower(packageMutationActions[cmd], strings.ToLower(name)) {
				return "system package mutation via " + cmd + " " + name, true
			}
			if strings.HasPrefix(value, "-") && !strings.HasPrefix(value, "--") && len(value) > 1 {
				if cmd == "dpkg" && strings.ContainsAny(value[1:], "irp") {
					return "system package mutation via dpkg " + value, true
				}
				if cmd == "rpm" && strings.ContainsRune("iUFe", rune(value[1])) {
					return "system package mutation via rpm " + value, true
				}
			}
		}
	}
	action, _, static := firstOperand(words, packageOptionValues)
	if !static {
		return "dynamic " + cmd + " action cannot be inspected safely", true
	}
	if containsLower(packageMutationActions[cmd], action) {
		if (cmd == "dnf" || cmd == "yum") && action == "module" {
			values, static := staticValues(words)
			if !static {
				return "dynamic " + cmd + " module action cannot be inspected safely", true
			}
			for i, value := range values {
				if strings.EqualFold(value, "module") && i+1 < len(values) {
					subaction := strings.ToLower(values[i+1])
					if !containsLower(lowerSet("enable", "disable", "install", "remove", "reset", "switch-to"), subaction) {
						return "", false
					}
				}
			}
		}
		return "system package mutation via " + cmd + " " + action, true
	}
	return "", false
}

func destructivePasswdMutation(words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic passwd action cannot be inspected safely", true
	}
	statusOnly := false
	for _, value := range values {
		if !strings.HasPrefix(value, "-") || value == "-" {
			continue
		}
		if value == "-S" || strings.EqualFold(value, "--status") {
			statusOnly = true
			continue
		}
		return "account database management via passwd", true
	}
	if statusOnly {
		return "", false
	}
	return "account database management via passwd", true
}

func destructiveChageMutation(words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic chage action cannot be inspected safely", true
	}
	listOnly := false
	for _, value := range values {
		if !strings.HasPrefix(value, "-") || value == "-" {
			continue
		}
		if value == "-l" || strings.EqualFold(value, "--list") {
			listOnly = true
			continue
		}
		return "account database management via chage", true
	}
	if listOnly {
		return "", false
	}
	return "account database management via chage", true
}

func destructivePacmanMutation(words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic pacman action cannot be inspected safely", true
	}
	syncMode := false
	queryOnly := false
	for _, value := range values {
		lower := strings.ToLower(value)
		switch lower {
		case "--remove", "--upgrade", "--sysupgrade", "--refresh", "--clean", "--database":
			return "system package mutation via pacman " + lower, true
		case "--sync":
			syncMode = true
		case "--search", "--info", "--list", "--groups", "--print":
			queryOnly = true
		}
		if len(lower) >= 2 && lower[0] == '-' && lower[1] != '-' {
			flags := strings.TrimPrefix(lower, "-")
			if strings.ContainsAny(flags, "ru") {
				return "system package mutation via pacman -" + flags, true
			}
			if strings.ContainsRune(flags, 's') {
				syncMode = true
				if strings.ContainsAny(flags, "silgp") && len(flags) > 1 {
					queryOnly = true
				}
				if strings.ContainsAny(flags, "yuc") {
					return "system package mutation via pacman -" + flags, true
				}
			}
		}
	}
	if syncMode && !queryOnly {
		return "system package mutation via pacman sync", true
	}
	return "", false
}

func destructiveDSCLMutation(words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic dscl action cannot be inspected safely", true
	}
	mutations := lowerSet("-create", "-createpl", "-delete", "-append", "-merge", "-change", "-changei", "-passwd")
	for _, value := range values {
		if containsLower(mutations, value) {
			return "account database mutation via dscl", true
		}
	}
	return "", false
}

func destructiveSysadminctlMutation(words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic sysadminctl action cannot be inspected safely", true
	}
	mutations := lowerSet("-adduser", "-deleteuser", "-resetpasswordfor", "-securetokenon", "-securetokenoff", "-autologin", "-guestaccount")
	for _, value := range values {
		if containsLower(mutations, value) {
			return "account database mutation via sysadminctl", true
		}
	}
	return "", false
}

func destructiveNetAccountMutation(words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic net account action cannot be inspected safely", true
	}
	if len(values) == 0 || (strings.ToLower(values[0]) != "user" && strings.ToLower(values[0]) != "localgroup") {
		return "", false
	}
	for _, value := range values[1:] {
		lower := strings.ToLower(value)
		if lower == "/add" || lower == "/delete" || strings.HasPrefix(lower, "/active:") || strings.HasPrefix(lower, "/passwordchg:") {
			return "account database mutation via net " + strings.ToLower(values[0]), true
		}
	}
	return "", false
}

func destructiveIPTablesMutation(cmd string, words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic firewall action cannot be inspected safely", true
	}
	shortMutations := map[string]struct{}{
		"-A": {}, "-D": {}, "-I": {}, "-R": {}, "-F": {}, "-Z": {},
		"-N": {}, "-X": {}, "-P": {}, "-E": {},
	}
	longMutations := lowerSet("--append", "--delete", "--insert", "--replace", "--flush", "--zero", "--new-chain", "--delete-chain", "--policy", "--rename-chain")
	for _, value := range values {
		name, _ := optionName(value)
		_, shortMutation := shortMutations[name]
		if shortMutation || containsLower(longMutations, strings.ToLower(name)) {
			return "firewall mutation via " + cmd + " " + name, true
		}
	}
	return "", false
}

func destructiveNFTMutation(words []shellWord) (string, bool) {
	action, _, static := firstOperand(words, lowerSet("-I", "--includepath", "-d", "--debug"))
	if !static {
		return "dynamic nft action cannot be inspected safely", true
	}
	if action == "" {
		return "", false
	}
	if action == "-f" || action == "--file" || containsLower(lowerSet("add", "delete", "insert", "replace", "flush", "reset", "rename", "import"), action) {
		return "firewall mutation via nft " + action, true
	}
	values, _ := staticValues(words)
	for _, value := range values {
		if value == "-f" || strings.HasPrefix(value, "--file") {
			return "firewall ruleset replacement via nft", true
		}
	}
	return "", false
}

func destructiveUFWMutation(words []shellWord) (string, bool) {
	action, _, static := firstOperand(words, nil)
	if !static {
		return "dynamic ufw action cannot be inspected safely", true
	}
	mutations := lowerSet("enable", "disable", "reset", "reload", "allow", "deny", "reject", "limit", "delete", "insert", "route", "default", "logging", "app")
	if containsLower(mutations, action) {
		return "firewall mutation via ufw " + action, true
	}
	return "", false
}

func destructiveFirewallCmdMutation(words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic firewall-cmd action cannot be inspected safely", true
	}
	for _, value := range values {
		lower := strings.ToLower(value)
		if lower == "--reload" || lower == "--complete-reload" || lower == "--runtime-to-permanent" || lower == "--panic-on" || lower == "--panic-off" ||
			strings.HasPrefix(lower, "--add-") || strings.HasPrefix(lower, "--remove-") || strings.HasPrefix(lower, "--change-") ||
			strings.HasPrefix(lower, "--set-") || strings.HasPrefix(lower, "--new-") || strings.HasPrefix(lower, "--delete-") || strings.HasPrefix(lower, "--load-") {
			return "firewall mutation via firewall-cmd " + lower, true
		}
		if lower == "--state" || lower == "--help" || lower == "--version" || lower == "--check-config" || lower == "--permanent" ||
			strings.HasPrefix(lower, "--zone") || strings.HasPrefix(lower, "--policy") || strings.HasPrefix(lower, "--service") ||
			strings.HasPrefix(lower, "--get-") || strings.HasPrefix(lower, "--list-") || strings.HasPrefix(lower, "--query-") || strings.HasPrefix(lower, "--info-") {
			continue
		}
		if strings.HasPrefix(lower, "-") {
			return "firewall mutation via firewall-cmd " + lower, true
		}
	}
	return "", false
}

func destructivePFCTLMutation(words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic pfctl action cannot be inspected safely", true
	}
	for _, value := range values {
		if value == "-e" || value == "-d" || value == "-f" || value == "-F" || strings.HasPrefix(value, "-f") || strings.HasPrefix(value, "-F") || strings.HasPrefix(value, "-x") {
			return "firewall mutation via pfctl " + value, true
		}
	}
	return "", false
}

func destructiveSystemctlMutation(words []shellWord) (string, bool) {
	for _, word := range words {
		if word.static && strings.EqualFold(word.value, "--user") {
			// Per-user units are part of ordinary local development and cannot
			// mutate the machine-wide service manager.
			return "", false
		}
	}
	action, _, static := firstOperand(words, lowerSet("--root", "--image", "--host", "-H", "--machine", "-M", "--type", "-t", "--state", "--property", "-p", "--signal", "-s"))
	if !static {
		return "dynamic systemctl action cannot be inspected safely", true
	}
	mutations := lowerSet("start", "stop", "restart", "try-restart", "reload", "reload-or-restart", "enable", "disable", "reenable", "preset", "preset-all", "mask", "unmask", "link", "revert", "set-default", "isolate", "daemon-reload", "edit", "set-property", "kill", "reset-failed", "add-wants", "add-requires")
	if containsLower(mutations, action) {
		return "service manager mutation via systemctl " + action, true
	}
	return "", false
}

func destructiveServiceMutation(words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic service action cannot be inspected safely", true
	}
	if len(values) < 2 {
		return "", false
	}
	action := strings.ToLower(values[1])
	if containsLower(lowerSet("start", "stop", "restart", "reload", "force-reload", "enable", "disable"), action) {
		return "service manager mutation via service " + action, true
	}
	return "", false
}

func destructiveLaunchctlMutation(words []shellWord) (string, bool) {
	action, _, static := firstOperand(words, nil)
	if !static {
		return "dynamic launchctl action cannot be inspected safely", true
	}
	if containsLower(lowerSet("bootstrap", "bootout", "load", "unload", "enable", "disable", "kickstart", "kill", "remove", "submit", "setenv", "unsetenv"), action) {
		return "service manager mutation via launchctl " + action, true
	}
	return "", false
}

func destructiveDockerMutation(words []shellWord) (string, bool) {
	action, index, static := firstOperand(words, lowerSet("--config", "--context", "-c", "--host", "-h", "--log-level"))
	if !static {
		return "dynamic docker action cannot be inspected safely", true
	}
	if action != "system" {
		return "", false
	}
	if index+1 >= len(words) {
		return "", false
	}
	if !words[index+1].static {
		return "dynamic docker system action cannot be inspected safely", true
	}
	if strings.EqualFold(words[index+1].value, "prune") {
		return "machine-wide Docker cleanup via docker system prune", true
	}
	return "", false
}

var kubectlOptionValues = lowerSet(
	"--context", "--namespace", "-n", "--kubeconfig", "--kuberc", "--cluster", "--user", "--server", "-s",
	"--request-timeout", "--proxy-url", "--cache-dir", "--as", "--as-group", "--as-uid", "--as-user-extra", "--token",
	"--certificate-authority", "--client-certificate", "--client-key", "--tls-server-name",
	"--username", "--password", "--profile", "--profile-output", "--vmodule",
	"--storage-driver-buffer-duration", "--storage-driver-db", "--storage-driver-host",
	"--storage-driver-password", "--storage-driver-table", "--storage-driver-user",
	"--log-flush-frequency", "--loglevel", "--v", "-v",
)

var clusterScopedResources = lowerSet(
	"namespace", "namespaces", "ns", "node", "nodes", "no",
	"clusterrole", "clusterroles", "clusterrolebinding", "clusterrolebindings",
	"customresourcedefinition", "customresourcedefinitions", "crd", "crds",
	"persistentvolume", "persistentvolumes", "pv", "pvs",
	"storageclass", "storageclasses", "sc",
	"volumeattachment", "volumeattachments", "csidriver", "csidrivers", "csinode", "csinodes",
	"mutatingwebhookconfiguration", "mutatingwebhookconfigurations",
	"validatingwebhookconfiguration", "validatingwebhookconfigurations",
	"validatingadmissionpolicy", "validatingadmissionpolicies",
	"validatingadmissionpolicybinding", "validatingadmissionpolicybindings",
	"apiservice", "apiservices",
	"certificatesigningrequest", "certificatesigningrequests", "csr",
	"clustertrustbundle", "clustertrustbundles",
	"ingressclass", "ingressclasses", "ipaddress", "ipaddresses", "servicecidr", "servicecidrs",
	"runtimeclass", "runtimeclasses", "podsecuritypolicy", "podsecuritypolicies", "psp",
	"priorityclass", "priorityclasses", "pc",
	"flowschema", "flowschemas", "prioritylevelconfiguration", "prioritylevelconfigurations",
	"deviceclass", "deviceclasses", "resourceslice", "resourceslices",
)

func destructiveKubectlMutation(words []shellWord) (string, bool) {
	verb, index, static := firstOperand(words, kubectlOptionValues)
	if !static {
		return "dynamic kubectl action cannot be inspected safely", true
	}
	if verb == "drain" || verb == "cordon" {
		return "cluster node disruption via kubectl " + verb, true
	}
	if verb != "delete" {
		return "", false
	}
	values, _ := staticValues(words)
	if values == nil {
		return "dynamic kubectl delete resource cannot be inspected safely", true
	}
	for _, value := range values[index+1:] {
		lower := strings.ToLower(value)
		name, _ := optionName(lower)
		if name == "-f" || name == "--filename" || name == "-k" || name == "--kustomize" ||
			(strings.HasPrefix(lower, "-f") && !strings.HasPrefix(lower, "--")) ||
			(strings.HasPrefix(lower, "-k") && !strings.HasPrefix(lower, "--")) {
			// The manifest can contain Namespace, ClusterRole, CRD, or any
			// other cluster-scoped resource. Its target cannot be proven safe
			// from the shell command alone, so delete-by-manifest fails closed.
			return "manifest-driven kubectl delete cannot be inspected safely", true
		}
		if lower == "--all-namespaces" || lower == "-a" ||
			(strings.HasPrefix(lower, "--all-namespaces=") && !strings.HasSuffix(lower, "=false")) ||
			(strings.HasPrefix(lower, "-a=") && !strings.HasSuffix(lower, "=false")) {
			return "cluster-wide kubectl delete", true
		}
	}
	resource, _, inspectable := firstOperand(words[index+1:], lowerSet("-f", "--filename", "-k", "--kustomize", "-l", "--selector", "--field-selector", "--grace-period", "--timeout", "--cascade", "--wait"))
	if !inspectable {
		return "dynamic kubectl delete resource cannot be inspected safely", true
	}
	if resource == "" {
		return "", false
	}
	resource = strings.SplitN(resource, "/", 2)[0]
	resource = strings.SplitN(resource, ".", 2)[0]
	for _, candidate := range strings.Split(resource, ",") {
		if containsLower(clusterScopedResources, candidate) {
			return "cluster-scoped kubectl delete of " + candidate, true
		}
	}
	return "", false
}

func destructiveCrontabMutation(words []shellWord) (string, bool) {
	values, static := staticValues(words)
	if !static {
		return "dynamic crontab action cannot be inspected safely", true
	}
	for _, value := range values {
		lower := strings.ToLower(value)
		if lower == "--remove" || (strings.HasPrefix(lower, "-") && !strings.HasPrefix(lower, "--") && strings.ContainsRune(lower[1:], 'r')) {
			return "destructive crontab removal", true
		}
	}
	return "", false
}

func staticWord(word *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescapeShellLiteral(p.Value, false))
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				switch q := inner.(type) {
				case *syntax.Lit:
					b.WriteString(unescapeShellLiteral(q.Value, true))
				case *syntax.SglQuoted:
					b.WriteString(q.Value)
				default:
					return "", false
				}
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// wordStaysSingleArg classifies argv cardinality independently from staticWord.
// Quoted dynamic values such as "$context" remain one argv entry, while
// unquoted expansions and literal glob/brace patterns may expand into multiple
// entries and inject the action firstOperand is trying to identify.
func wordStaysSingleArg(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			// A literal adjacent to a quoted expansion is still one word unless
			// it introduces an unquoted glob/brace expansion.
			if strings.ContainsAny(p.Value, "*?[]{}") {
				return false
			}
		case *syntax.SglQuoted:
			// Literal and single-quoted parts cannot change argv cardinality.
		case *syntax.DblQuoted:
			if !doubleQuotedStaysSingleArg(p) {
				return false
			}
		default:
			// Unquoted parameter/command/arithmetic expansions may split.
			return false
		}
	}
	return true
}

func doubleQuotedStaysSingleArg(quoted *syntax.DblQuoted) bool {
	if quoted == nil {
		return false
	}
	for _, part := range quoted.Parts {
		switch p := part.(type) {
		case *syntax.ParamExp:
			// "$@", arrays, and name enumeration are the exceptions where a
			// double-quoted parameter expansion can still yield multiple argv
			// entries. Plain scalar parameters remain exactly one entry.
			if p.Param == nil || p.Param.Value == "@" || p.Index != nil || p.Names != 0 || p.Flags != nil || p.NestedParam != nil {
				return false
			}
		case *syntax.DblQuoted:
			if !doubleQuotedStaysSingleArg(p) {
				return false
			}
		}
	}
	return true
}

// mvdan preserves shell escapes in Lit.Value. Resolve only the escapes that
// the shell removes at execution time; single-quoted words bypass this helper
// and therefore keep backslashes as ordinary data.
func unescapeShellLiteral(value string, doubleQuoted bool) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			b.WriteByte(value[i])
			continue
		}
		next := value[i+1]
		if doubleQuoted && !strings.ContainsRune(`$\"`+"\n", rune(next)) {
			b.WriteByte(value[i])
			continue
		}
		if next != '\n' {
			b.WriteByte(next)
		}
		i++
	}
	return b.String()
}

func commandBase(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return strings.ToLower(filepath.Base(raw))
}

func isKillCommand(cmd string) bool {
	switch cmd {
	case "kill", "pkill", "killall", "taskkill", "taskkill.exe", "stop-process", "spps":
		return true
	default:
		return false
	}
}

func commandTarget(words []shellWord, options map[string]bool) ([]shellWord, error) {
	for len(words) > 0 {
		if !words[0].static {
			return nil, blocked("dynamic wrapper option or command cannot be inspected safely")
		}
		word := words[0].value
		if word == "--" {
			return words[1:], nil
		}
		if !strings.HasPrefix(word, "-") || word == "-" {
			return words, nil
		}
		name, hasInlineValue := optionName(word)
		needsValue, known := options[name]
		if !known {
			return nil, blocked("unknown wrapper option " + word + " makes the command target ambiguous")
		}
		words = words[1:]
		if needsValue && !hasInlineValue {
			if len(words) == 0 || !words[0].static {
				return nil, blocked("wrapper option " + name + " has an uninspectable value")
			}
			words = words[1:]
		}
	}
	return nil, nil
}

func optionName(word string) (name string, hasInlineValue bool) {
	if i := strings.IndexByte(word, '='); i >= 0 {
		return word[:i], true
	}
	return word, false
}

func inspectEnv(words []shellWord, depth int) error {
	for len(words) > 0 {
		if !words[0].static {
			return blocked("dynamic env option, assignment, or command cannot be inspected safely")
		}
		word := words[0].value
		if word == "--" {
			return inspectWords(words[1:], depth)
		}
		if strings.Contains(word, "=") && !strings.HasPrefix(word, "-") {
			words = words[1:]
			continue
		}
		name, inline := optionName(word)
		switch name {
		case "-S", "--split-string":
			var payload string
			if inline {
				payload = word[strings.IndexByte(word, '=')+1:]
				words = words[1:]
			} else {
				if len(words) < 2 || !words[1].static {
					return blocked("env split-string payload cannot be inspected safely")
				}
				payload = words[1].value
				words = words[2:]
			}
			if err := check(payload, depth+1); err != nil {
				return err
			}
			continue
		case "-u", "--unset", "-C", "--chdir":
			words = words[1:]
			if !inline {
				if len(words) == 0 || !words[0].static {
					return blocked("env option " + name + " has an uninspectable value")
				}
				words = words[1:]
			}
			continue
		case "-i", "--ignore-environment", "-0", "--null", "-v", "--debug":
			words = words[1:]
			continue
		}
		if strings.HasPrefix(word, "-") && word != "-" {
			return blocked("unknown env option " + word + " makes the command target ambiguous")
		}
		return inspectWords(words, depth)
	}
	return nil
}

func inspectNice(words []shellWord, depth int) error {
	for len(words) > 0 {
		if !words[0].static {
			return blocked("dynamic nice option or command cannot be inspected safely")
		}
		word := words[0].value
		if word == "--" {
			return inspectWords(words[1:], depth)
		}
		if word == "-n" || word == "--adjustment" {
			if len(words) < 2 || !words[1].static {
				return blocked("nice adjustment cannot be inspected safely")
			}
			words = words[2:]
			continue
		}
		if strings.HasPrefix(word, "--adjustment=") || isSignedNumberOption(word) {
			words = words[1:]
			continue
		}
		if strings.HasPrefix(word, "-") && word != "-" {
			return blocked("unknown nice option " + word + " makes the command target ambiguous")
		}
		return inspectWords(words, depth)
	}
	return nil
}

func isSignedNumberOption(word string) bool {
	if len(word) < 2 || (word[0] != '-' && word[0] != '+') {
		return false
	}
	for _, r := range word[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func inspectTimeout(words []shellWord, depth int) error {
	target, err := commandTarget(words, map[string]bool{
		"-k": true, "--kill-after": true, "-s": true, "--signal": true,
		"--foreground": false, "--preserve-status": false, "--verbose": false,
		"--help": false, "--version": false,
	})
	if err != nil {
		return err
	}
	if len(target) == 0 {
		return nil
	}
	if !target[0].static {
		return blocked("dynamic timeout duration cannot be inspected safely")
	}
	return inspectWords(target[1:], depth)
}

func inspectXargs(words []shellWord, depth int) error {
	for len(words) > 0 {
		if !words[0].static {
			return blocked("dynamic xargs option or command cannot be inspected safely")
		}
		word := words[0].value
		if word == "--" {
			return inspectWords(words[1:], depth)
		}
		if !strings.HasPrefix(word, "-") || word == "-" {
			return inspectWords(words, depth)
		}
		name, inline := optionName(word)
		if isAttachedXargsOption(word) {
			words = words[1:]
			continue
		}
		switch name {
		case "-a", "--arg-file", "-d", "--delimiter", "-E", "--eof", "-I", "-J", "--max-replsize", "-L", "--max-lines", "-n", "--max-args", "-P", "--max-procs", "-R", "-S", "-s", "--max-chars", "--process-slot-var":
			words = words[1:]
			if !inline {
				if len(words) == 0 || !words[0].static {
					return blocked("xargs option " + name + " has an uninspectable value")
				}
				words = words[1:]
			}
			continue
		case "--replace":
			if inline {
				words = words[1:]
				continue
			}
			// GNU permits an optional replacement string. Both interpretations
			// must be safe because platform variants disagree about consumption.
			if err := inspectWords(words[1:], depth); err != nil {
				return err
			}
			if len(words) > 2 {
				return inspectWords(words[2:], depth)
			}
			return nil
		case "-0", "--null", "-o", "--open-tty", "-p", "--interactive", "-r", "--no-run-if-empty", "-t", "--verbose", "-x", "--exit", "--show-limits":
			words = words[1:]
			continue
		default:
			return blocked("unknown xargs option " + word + " makes the command target ambiguous")
		}
	}
	return nil
}

func isAttachedXargsOption(word string) bool {
	if len(word) <= 2 || word[0] != '-' || word[1] == '-' {
		return false
	}
	return strings.ContainsRune("aEdIJLnPRSs", rune(word[1]))
}

func inspectFind(words []shellWord, depth int) error {
	for i := 0; i < len(words); i++ {
		if !words[i].static {
			continue
		}
		switch words[i].value {
		case "-exec", "-execdir", "-ok", "-okdir":
			start := i + 1
			end := start
			for end < len(words) {
				if !words[end].static {
					return blocked("dynamic find action cannot be inspected safely")
				}
				if words[end].value == ";" || words[end].value == "+" {
					break
				}
				end++
			}
			if end == len(words) {
				return blocked("unterminated find action cannot be inspected safely")
			}
			if err := inspectWords(words[start:end], depth); err != nil {
				return err
			}
			i = end
		}
	}
	return nil
}

func joinStatic(words []shellWord) (string, error) {
	parts := make([]string, 0, len(words))
	for _, word := range words {
		if !word.static {
			return "", blocked("dynamic shell payload cannot be inspected safely")
		}
		parts = append(parts, word.value)
	}
	return strings.Join(parts, " "), nil
}

func staticShellPayload(words []shellWord) (string, bool, error) {
	for i := 0; i < len(words); i++ {
		if !words[i].static {
			return "", false, blocked("dynamic shell option or payload cannot be inspected safely")
		}
		word := words[i].value
		if word == "--" {
			return "", false, nil
		}
		if word == "-O" || word == "+O" {
			i++
			if i >= len(words) || !words[i].static {
				return "", false, blocked("shell option value cannot be inspected safely")
			}
			continue
		}
		if !strings.HasPrefix(word, "-") || word == "-" {
			return "", false, nil
		}
		if strings.Contains(strings.TrimLeft(word, "-+"), "c") {
			if i+1 >= len(words) || !words[i+1].static {
				return "", false, blocked("dynamic shell -c payload cannot be inspected safely")
			}
			return words[i+1].value, true, nil
		}
	}
	return "", false, nil
}

func inspectCmd(words []shellWord, depth int) error {
	for i, word := range words {
		if !word.static {
			return blocked("dynamic cmd.exe payload cannot be inspected safely")
		}
		if strings.EqualFold(word.value, "/c") || strings.EqualFold(word.value, "/k") {
			payload, err := joinStatic(words[i+1:])
			if err != nil {
				return err
			}
			return check(payload, depth+1)
		}
	}
	return nil
}

func inspectPowerShell(words []shellWord, depth int) error {
	for i, word := range words {
		if !word.static {
			return blocked("dynamic PowerShell payload cannot be inspected safely")
		}
		name := strings.ToLower(word.value)
		switch name {
		case "-encodedcommand", "-enc", "/encodedcommand":
			return blocked("encoded PowerShell command cannot be inspected safely")
		case "-command", "-c", "/command":
			payload, err := joinStatic(words[i+1:])
			if err != nil {
				return err
			}
			return check(payload, depth+1)
		}
	}
	return nil
}

var sudoOptions = map[string]bool{
	"-u": true, "--user": true, "-g": true, "--group": true, "-h": true, "--host": true,
	"-C": true, "--close-from": true, "-p": true, "--prompt": true, "-R": true, "--chroot": true,
	"-T": true, "--command-timeout": true, "-r": true, "--role": true, "-t": true, "--type": true,
	"-n": false, "--non-interactive": false, "-E": false, "--preserve-env": false,
	"-H": false, "--set-home": false, "-S": false, "--stdin": false, "-k": false, "-K": false,
}

var parallelOptions = map[string]bool{
	"-j": true, "--jobs": true, "-S": true, "--sshlogin": true, "--results": true, "--joblog": true,
}
