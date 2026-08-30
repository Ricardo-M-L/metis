package shellguard

import (
	"strings"
	"testing"
)

func TestCheckBlocksProcessTerminationCommands(t *testing.T) {
	t.Parallel()
	cases := []string{
		`kill -9 123`,
		`/bin/kill -TERM 123`,
		`pkill metis`,
		`/usr/bin/killall -9 metis`,
		`ps aux | awk '{print $2}' | xargs kill -9`,
		`printf '1\n2\n' | xargs -n 1 /bin/kill -TERM`,
		`command kill -9 123`,
		`builtin kill -TERM 123`,
		`exec /bin/kill 123`,
		`env FOO=bar pkill metis`,
		`sudo --non-interactive killall metis`,
		`env -u TOKEN sudo -u root /bin/kill 123`,
		`parallel --jobs 2 kill ::: 123 456`,
		`parallel 'kill {}' ::: 123`,
		`sh -c 'kill -9 123'`,
		`bash -lc 'pkill metis'`,
		`env zsh -c 'killall metis'`,
		`sh -c "echo safe; /bin/kill 123"`,
		`echo before && pkill metis`,
		`echo before || killall metis`,
		`(echo before; kill 123)`,
		`if true; then pkill metis; fi`,
		`for pid in 1 2; do kill "$pid"; done`,
		`echo "pid=$(kill -0 123)"`,
		`timeout 10 kill -9 123`,
		`nice -n 5 /bin/kill 123`,
		`find . -exec kill -9 {} \;`,
		`find . -execdir /bin/kill {} +`,
		`busybox kill -9 123`,
		`toybox kill 123`,
		`eval 'kill -9 123'`,
		`bash -O extglob -c 'kill -9 123'`,
		`cmd.exe /c taskkill /PID 123`,
		`pwsh -Command 'Stop-Process -Id 123'`,
		`pwsh -Command '& { Stop-Process -Id 123 }'`,
		`cmd.exe /c 'if exist marker (taskkill /PID 123)'`,
		`powershell -EncodedCommand Zm9v`,
		`killer=kill; "$killer" -9 123`,
		`bash -c "$payload"`,
		`\kill -9 123`,
		`k\ill -9 123`,
		`k* -9 123`,
		`k[il]l -9 123`,
	}
	for _, command := range cases {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			err := Check(command)
			if err == nil {
				t.Fatalf("Check(%q) allowed a process termination command", command)
			}
			if !strings.Contains(err.Error(), "BashKill(job_id)") {
				t.Fatalf("Check(%q) error %q does not direct the model to BashKill(job_id)", command, err)
			}
		})
	}
}

func TestCheckAllowsProcessWordsUsedAsData(t *testing.T) {
	t.Parallel()
	cases := []string{
		`echo 'kill -9 123'`,
		`printf '%s\n' "pkill metis"`,
		`grep -R "killall" .`,
		`rg 'kill -9|pkill|killall' internal`,
		`command -v kill`,
		`command -V pkill`,
		`type killall`,
		`which kill`,
		`ps aux | grep metis`,
		`parallel echo kill ::: one two`,
		`sh -c 'echo "kill -9"'`,
		`docker kill container-name`,
		`/tmp/kill-helper --version`,
		`killjoy --version`,
		`exec -a kill echo ok`,
		`sudo -u kill echo ok`,
		`xargs -I kill echo kill`,
		`grep -R '\kill' .`,
		`printf '%s' 'k\ill'`,
		`echo '$((1 + 2))'`,
	}
	for _, command := range cases {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err != nil {
				t.Fatalf("Check(%q) = %v, want allowed text/search use", command, err)
			}
		})
	}
}

func TestCheckBlocksDestructiveSystemMutations(t *testing.T) {
	t.Parallel()
	cases := []string{
		`/usr/bin/apt-get install nginx`,
		`sudo -n /usr/bin/dnf remove old-package`,
		`command env LANG=C /sbin/apk upgrade`,
		`echo ready && /usr/bin/zypper remove nginx`,
		`/usr/bin/pacman -Syu`,
		`dnf module enable nodejs:20`,
		`/usr/bin/dpkg -i package.deb`,
		`/usr/bin/rpm -Uvh package.rpm`,
		`/usr/sbin/useradd test-user`,
		`sudo /usr/sbin/groupdel test-group`,
		`command /usr/bin/passwd test-user`,
		`env LANG=C /usr/sbin/iptables -F`,
		`/usr/sbin/nft flush ruleset`,
		`sudo ufw disable`,
		`firewall-cmd --reload`,
		`systemctl stop sshd.service`,
		`systemctl kill sshd.service`,
		`/usr/bin/docker system prune -af`,
		`docker --context production system prune --volumes`,
		`kubectl delete namespace production`,
		`/usr/local/bin/kubectl --context production delete clusterrole admin`,
		`kubectl delete pods --all --all-namespaces`,
		`kubectl delete -f manifest.yaml`,
		`kubectl delete -f=manifest.yaml`,
		`kubectl delete --filename=manifest.yaml`,
		`sudo /usr/local/bin/kubectl delete -k overlays/production`,
		`command env KUBECONFIG=/tmp/config /opt/bin/kubectl delete --kustomize=overlays/production`,
		`crontab -r`,
		`sudo crontab -u root -r`,
		`bash -lc 'echo ready; /usr/sbin/userdel test-user'`,
		`action=install; apt "$action" nginx`,
		`action=prune; docker system "$action"`,
		`resource=namespace; kubectl delete "$resource" production`,
	}
	for _, command := range cases {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err == nil {
				t.Fatalf("Check(%q) allowed a destructive system mutation", command)
			}
		})
	}
}

func TestCheckAllowsSystemQueriesAndScopedDevelopmentOperations(t *testing.T) {
	t.Parallel()
	cases := []string{
		`/usr/bin/apt list --installed`,
		`apt show install`,
		`dnf list installed`,
		`apk info nginx`,
		`pacman -Ss curl`,
		`zypper search nginx`,
		`dpkg --list`,
		`rpm -qi installed-package`,
		`dnf module list`,
		`passwd -S test-user`,
		`chage -l test-user`,
		`command -v useradd`,
		`getent passwd test-user`,
		`/usr/sbin/iptables -L -n`,
		`nft list ruleset`,
		`ufw status verbose`,
		`firewall-cmd --state`,
		`firewall-cmd --list-all`,
		`systemctl status sshd.service`,
		`systemctl show sshd.service`,
		`systemctl --user restart local-development.service`,
		`docker ps`,
		`docker build -t example .`,
		`docker container rm stopped-container`,
		`docker run alpine echo system prune`,
		`kubectl get namespaces`,
		`kubectl delete pod one-broken-pod`,
		`kubectl apply -f deployment.yaml`,
		`kubectl rollout restart deployment/web`,
		`resource=pods; kubectl get "$resource"`,
		`context=development; kubectl --context "$context" get pods`,
		`context=development; docker --context "$context" build -t local/test .`,
		`package=nginx; apt show "$package"`,
		`crontab -l`,
		`printf '%s\n' 'docker system prune'`,
		`echo 'kubectl delete namespace production'`,
	}
	for _, command := range cases {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err != nil {
				t.Fatalf("Check(%q) rejected an allowed operation: %v", command, err)
			}
		})
	}
}

func TestCheckParsesKubectlVerbosityFlagsBeforeTheVerb(t *testing.T) {
	t.Parallel()

	blocked := []string{
		`kubectl --v 0 delete namespace production`,
		`kubectl --v=0 delete namespace production`,
		`kubectl -v 0 delete namespace production`,
		`kubectl -v=0 delete namespace production`,
		`kubectl --v * delete pods`,
	}
	for _, command := range blocked {
		command := command
		t.Run("blocks "+command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err == nil {
				t.Fatalf("Check(%q) allowed a destructive system mutation", command)
			}
		})
	}

	allowed := []string{
		`kubectl --v 0 get namespaces`,
		`kubectl --v=0 get pods`,
		`kubectl -v 0 get pods`,
		`kubectl -v=0 get pods`,
		`kubectl --v "$verbosity" get pods`,
		`kubectl --v "${verbosity:-0}" get pods`,
		`kubectl -v "$verbosity" get pods`,
		`kubectl --v "*" get pods`,
	}
	for _, command := range allowed {
		command := command
		t.Run("allows "+command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err != nil {
				t.Fatalf("Check(%q) rejected an allowed operation: %v", command, err)
			}
		})
	}

	uninspectable := []string{
		`kubectl --v $verbosity get pods`,
		`kubectl -v $verbosity get pods`,
		`kubectl --v "$@" get pods`,
		`kubectl --v "${verbosity_levels[@]}" get pods`,
		`kubectl --v * get pods`,
		`kubectl --v ? get pods`,
		`kubectl --v [0-9] get pods`,
		`kubectl --v {0,delete,namespace,production}`,
	}
	for _, command := range uninspectable {
		command := command
		t.Run("rejects "+command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err == nil {
				t.Fatalf("Check(%q) allowed an option value that can expand into multiple arguments", command)
			}
		})
	}
}

func TestCheckParsesKubectlGlobalValueFlagsBeforeTheVerb(t *testing.T) {
	t.Parallel()

	blocked := []string{
		`kubectl --username alice delete namespace production`,
		`kubectl --password placeholder delete namespace production`,
		`kubectl --profile cpu delete namespace production`,
		`kubectl --profile-output profile.out delete namespace production`,
		`kubectl --vmodule client=2 delete namespace production`,
		`kubectl --log-flush-frequency 5s delete namespace production`,
		`kubectl --as-user-extra team=platform delete namespace production`,
		`kubectl --kuberc preferences.yaml delete namespace production`,
		`kubectl --proxy-url http://proxy.invalid delete namespace production`,
		`kubectl --storage-driver-buffer-duration 1m delete namespace production`,
		`kubectl --storage-driver-db metrics delete namespace production`,
		`kubectl --storage-driver-host localhost:8086 delete namespace production`,
		`kubectl --storage-driver-password placeholder delete namespace production`,
		`kubectl --storage-driver-table stats delete namespace production`,
		`kubectl --storage-driver-user root delete namespace production`,
		`oc --username alice delete namespace production`,
		`oc --loglevel 8 delete namespace production`,
		`kubectl --username=alice delete namespace production`,
		`kubectl --as-user-extra=team=platform delete namespace production`,
		`kubectl --kuberc=preferences.yaml delete namespace production`,
		`kubectl --proxy-url=http://proxy.invalid delete namespace production`,
		`kubectl --storage-driver-db=metrics delete namespace production`,
		`oc --loglevel=8 delete namespace production`,
		`kubectl --disable-compression delete namespace production`,
		`kubectl --insecure-skip-tls-verify delete namespace production`,
		`kubectl --match-server-version delete namespace production`,
		`kubectl --storage-driver-secure delete namespace production`,
		`kubectl --warnings-as-errors delete namespace production`,
	}
	for _, command := range blocked {
		command := command
		t.Run("blocks "+command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err == nil {
				t.Fatalf("Check(%q) allowed a destructive system mutation", command)
			}
		})
	}

	allowed := []string{
		`kubectl --username alice get pods`,
		`kubectl --profile-output profile.out get pods`,
		`kubectl --as-user-extra team=platform get pods`,
		`kubectl --kuberc preferences.yaml get pods`,
		`kubectl --proxy-url http://proxy.invalid get pods`,
		`kubectl --storage-driver-db metrics get pods`,
		`oc --profile cpu get pods`,
		`oc --loglevel 8 get pods`,
		`kubectl --username=alice get pods`,
		`kubectl --as-user-extra=team=platform get pods`,
		`kubectl --kuberc=preferences.yaml get pods`,
		`kubectl --proxy-url=http://proxy.invalid get pods`,
		`kubectl --storage-driver-db=metrics get pods`,
		`oc --loglevel=8 get pods`,
	}
	for _, command := range allowed {
		command := command
		t.Run("allows "+command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err != nil {
				t.Fatalf("Check(%q) rejected an allowed operation: %v", command, err)
			}
		})
	}

	uninspectable := []string{
		`kubectl --username $identity get pods`,
		`kubectl --as-user-extra $extra get pods`,
		`kubectl --kuberc $preferences get pods`,
		`kubectl --proxy-url $proxy get pods`,
		`kubectl --storage-driver-db $database get pods`,
		`oc --profile $profile get pods`,
		`oc --loglevel $level get pods`,
	}
	for _, command := range uninspectable {
		command := command
		t.Run("rejects "+command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err == nil {
				t.Fatalf("Check(%q) allowed a dynamic option value", command)
			}
		})
	}
}

func TestCheckRejectsKubectlAllNamespacesBooleanShorthand(t *testing.T) {
	t.Parallel()

	for _, command := range []string{
		`kubectl delete pods --all -A=true`,
		`kubectl delete pods --all -A=TRUE`,
		`oc delete pods --all -A=true`,
	} {
		command := command
		t.Run("blocks "+command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err == nil {
				t.Fatalf("Check(%q) allowed a cluster-wide delete", command)
			}
		})
	}

	for _, command := range []string{
		`kubectl delete pod one-broken-pod -A=false`,
		`oc delete pod one-broken-pod -A=FALSE`,
	} {
		command := command
		t.Run("allows "+command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err != nil {
				t.Fatalf("Check(%q) rejected an explicitly namespaced delete: %v", command, err)
			}
		})
	}
}

func TestCheckRejectsBuiltInClusterScopedKubectlResources(t *testing.T) {
	t.Parallel()

	resources := []string{
		"csr",
		"certificatesigningrequests.certificates.k8s.io",
		"ingressclasses.networking.k8s.io",
		"csidrivers.storage.k8s.io",
		"csinodes.storage.k8s.io",
		"volumeattachments.storage.k8s.io",
		"runtimeclasses.node.k8s.io",
		"flowschemas.flowcontrol.apiserver.k8s.io",
		"prioritylevelconfigurations.flowcontrol.apiserver.k8s.io",
		"validatingadmissionpolicies.admissionregistration.k8s.io",
		"validatingadmissionpolicybindings.admissionregistration.k8s.io",
		"clustertrustbundles.certificates.k8s.io",
		"servicecidrs.networking.k8s.io",
		"deviceclasses.resource.k8s.io",
		"resourceslices.resource.k8s.io",
	}
	for _, resource := range resources {
		resource := resource
		t.Run(resource, func(t *testing.T) {
			t.Parallel()
			command := "kubectl delete " + resource + " --all"
			if err := Check(command); err == nil {
				t.Fatalf("Check(%q) allowed deletion of a cluster-scoped resource", command)
			}
		})
	}
}

func TestCheckRejectsActionWordsThatCanExpandIntoMultipleArguments(t *testing.T) {
	t.Parallel()

	uninspectable := []string{
		`kubectl {delete,get} namespace production`,
		`kubectl * namespace production`,
		`apt {install,show} nginx`,
		`docker {system,ps} prune`,
	}
	for _, command := range uninspectable {
		command := command
		t.Run("rejects "+command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err == nil {
				t.Fatalf("Check(%q) allowed an action word that can expand into multiple arguments", command)
			}
		})
	}

	quotedLiterals := []string{
		`kubectl "{delete,get}" namespace production`,
		`kubectl "*" namespace production`,
		`apt "{install,show}" nginx`,
		`docker "{system,ps}" prune`,
	}
	for _, command := range quotedLiterals {
		command := command
		t.Run("allows "+command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err != nil {
				t.Fatalf("Check(%q) rejected a quoted single-argument action: %v", command, err)
			}
		})
	}
}
