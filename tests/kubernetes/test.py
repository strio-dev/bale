"""Run by the NixOS test driver with a Nix-provided ``config`` dictionary."""

import base64
import json
import re
import shlex
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path


ROOT = "/var/lib/bale-acceptance"
SOURCE = "192.168.1.1:5001"
DESTINATION = "192.168.1.1:5002"
PLUGINS = ROOT + "/plugins"
TOKEN = "acceptance-private-bearer-token"
REGISTRY_USER = "bale-acceptance"
REGISTRY_PASSWORD = "synthetic-private-registry-password"
REGISTRY_CA = ROOT + "/registry-tls/ca.crt"
REGISTRY_CONFIG = ROOT + "/registry-auth.json"
BAD_REGISTRY_CONFIG = ROOT + "/bad-registry-auth.json"
HELM_CONFIG = ROOT + "/helm-config"
HELM_REGISTRY_CONFIG = HELM_CONFIG + "/registry/config.json"
PULL_VALUES = ROOT + "/pull-values.json"
REGISTRY_FLAGS = " --ca-file " + REGISTRY_CA
SERVICE_HOST = "bale-flux-bale-flux.default.svc"
cases = []


def quote(value):
    return shlex.quote(str(value))


def run(command, timeout=180):
    return machine.succeed(command + " 2>&1", timeout=timeout)


def write(path, content):
    # The driver's shared-filesystem transfer also handles large CRD manifests
    # without putting their contents into a shell argument or the command log.
    with tempfile.TemporaryDirectory() as directory:
        source = Path(directory) / "input"
        source.write_text(content)
        machine.copy_from_host(str(source), path)


def read_json(command):
    return json.loads(run(command))


def get(kind, name):
    return read_json("kubectl get " + kind + " " + quote(name) + " -o json")


def apply(resource):
    write(ROOT + "/resource.json", json.dumps(resource))
    run("kubectl apply -f " + ROOT + "/resource.json")


def evidence(name, data):
    write(ROOT + "/evidence/" + name + ".json", json.dumps(data, indent=2))


def wait_json(command, predicate, timeout=180):
    deadline = time.monotonic() + timeout
    latest = None
    while time.monotonic() < deadline:
        code, output = machine.execute(command, timeout=30)
        if code == 0:
            latest = json.loads(output)
            if predicate(latest):
                return latest
        time.sleep(2)
    raise AssertionError("Timed out: " + command + "\n" + repr(latest))


def ready(resource):
    return any(c["type"] == "Ready" and c["status"] == "True"
               for c in resource.get("status", {}).get("conditions", []))


def helm_command(arguments, registry_config=HELM_REGISTRY_CONFIG):
    return "HELM_PLUGINS=" + PLUGINS + " HELM_CONFIG_HOME=" + HELM_CONFIG + " HELM_REGISTRY_CONFIG=" + registry_config + " DOCKER_CONFIG=" + ROOT + "/no-docker-credentials helm " + arguments


def helm(arguments):
    return run(helm_command(arguments))


def app_check(release, version, label, override_main=None):
    hook_expected = versions[version]["destination_image"]
    expected = override_main["destination_image"] if override_main else hook_expected
    main_digest = override_main["digest"] if override_main else versions[version]["image_digest"]
    main_version = override_main["version"] if override_main else version
    helper = hook_expected.split("@")[0] + "@" + versions["B"]["image_digest"]
    wait_json("kubectl get deployment " + release + " -o json",
              lambda d: d["spec"]["template"]["spec"]["containers"][0]["image"] == expected)
    run("kubectl rollout status deployment/" + release + " --timeout=90s", 120)
    wait_json("kubectl get job " + release + "-hook -o json",
              lambda j: j["spec"]["template"]["spec"]["containers"][0]["image"] == hook_expected)
    run("kubectl wait --for=condition=complete job/" + release + "-hook --timeout=90s", 120)
    pods = read_json("kubectl get pods -l app=" + release + " -o json")
    active = [p for p in pods["items"] if not p["metadata"].get("deletionTimestamp")
              and p["status"].get("phase") == "Running"]
    assert len(active) == 1, pods
    pod = active[0]
    assert pod["spec"].get("imagePullSecrets") == [{"name": "bale-pull-auth"}], pod
    assert pod["spec"]["containers"][0]["imagePullPolicy"] == "Always", pod
    assert pod["spec"]["initContainers"][0]["imagePullPolicy"] == "Always", pod
    assert pod["spec"]["containers"][0]["image"] == expected, pod
    assert pod["spec"]["initContainers"][0]["image"] == helper, pod
    assert main_digest in pod["status"]["containerStatuses"][0]["imageID"], pod
    assert versions["B"]["image_digest"] in pod["status"]["initContainerStatuses"][0]["imageID"], pod
    hooks = read_json("kubectl get pods -l job-name=" + release + "-hook -o json")["items"]
    hooks = [p for p in hooks if not p["metadata"].get("deletionTimestamp")]
    assert len(hooks) == 1, hooks
    hook = hooks[0]
    assert hook["spec"].get("imagePullSecrets") == [{"name": "bale-pull-auth"}], hook
    assert hook["spec"]["containers"][0]["imagePullPolicy"] == "Always", hook
    assert hook["spec"]["containers"][0]["image"] == hook_expected, hook
    assert versions[version]["image_digest"] in hook["status"]["containerStatuses"][0]["imageID"], hook
    assert run("kubectl logs " + quote(pod["metadata"]["name"]) + " -c app").strip() == main_version
    evidence(label, {"pod": pod, "hook": hook})
    cases.append({"case": label, "version": main_version, "main": expected, "init": helper})
    return pod


def registry_offline(port):
    trust = " --cacert " + REGISTRY_CA if port == 5002 else ""
    code, _ = machine.execute("curl -fsS --max-time 2" + trust + " " + ("https" if port == 5002 else "http") + "://192.168.1.1:" + str(port) + "/v2/")
    assert code == 7, code


def fixture(action, arguments=""):
    return run(quote(config["fixture"]) + " " + action + " --source " + SOURCE
               + " --dest " + DESTINATION + " --dir " + ROOT
               + " --registry-ca " + REGISTRY_CA + " --registry-config " + REGISTRY_CONFIG + " " + arguments)


def cli(arguments, label):
    """Keep public CLI JSON separate from its progress and error evidence."""
    report = ROOT + "/evidence/" + label + ".json"
    command = ("XDG_DATA_HOME=" + ROOT + "/data SSL_CERT_FILE=" + REGISTRY_CA + " DOCKER_CONFIG=" + ROOT
               + "/docker " + quote(config["bale"]) + " " + arguments)
    write(ROOT + "/evidence/" + label + ".command", command + "\n")
    machine.succeed(command + " >" + report + " 2>" + ROOT + "/evidence/" + label + ".log", timeout=300)
    return read_json("cat " + report)


def edit_delivery(created, path, repositories):
    # Edit the actual generated YAML rather than constructing a parallel plan.
    # The encoder emits canonical repository@digest strings without YAML quoting.
    text = run("cat " + path)
    rows = {row["ref"]: row["path"] for row in created["delivery"]["artifacts"]}
    assert "bundle: " + created["descriptor"]["digest"] in text, text
    current = None
    seen = set()
    output = []
    for line in text.splitlines():
        ref = re.fullmatch(r"\s*- ref: (.+)", line)
        if ref:
            current = ref.group(1)
            assert current in rows and current not in seen, current
        placement = re.fullmatch(r"(\s*)path: (.+)", line)
        if placement:
            assert current is not None and current in rows and placement.group(2) == rows[current], line
            repository = current.split("@", 1)[0]
            assert repository in repositories, repository
            line = placement.group(1) + "path: " + repositories[repository]
            seen.add(current)
        output.append(line)
    assert seen == set(rows), (seen, rows)
    write(path + ".generated", text)
    write(path, "\n".join(output) + "\n")


def capture(name, recipe, repositories, expected_plugin=True, source_hosts=()):
    recipe_path = ROOT + "/" + name + ".recipe.yaml"
    delivery_path = ROOT + "/" + name + ".delivery.yaml"
    layout = ROOT + "/capture-" + name
    write(recipe_path, json.dumps(recipe, indent=2))
    flags = "".join(" --plain-http " + quote(host) for host in source_hosts)
    created = cli("create --layout --plain-http " + SOURCE + flags + " -f " + recipe_path
                  + " " + layout + " --output-delivery " + delivery_path + " --format json",
                  "cli-create-" + name)
    assert not created["published"] and created["layoutPath"] == layout, created
    if expected_plugin:
        assert len(created["metadata"]["plugins"]) == 1, created
        selected = created["metadata"]["plugins"][0]
        assert selected["ref"] == discovery_ref, selected
        assert selected["descriptor"]["digest"] == discovery["descriptor"]["digest"], selected
        assert selected["repository"] == recipe["include"][0]["ref"].rsplit(":", 1)[0], selected
        assert any(origin["descriptor"]["digest"] == selected["descriptor"]["digest"]
                   for origin in created["metadata"]["origins"]), created
    else:
        assert not created["metadata"].get("plugins"), created
    edit_delivery(created, delivery_path, repositories)
    run("cp " + recipe_path + " " + delivery_path + " " + delivery_path + ".generated " + ROOT + "/evidence/")
    saved = cli("save --layout " + layout + " -o " + ROOT + "/" + name + ".tar --format json", "cli-save-" + name)
    assert saved["descriptor"]["digest"] == created["descriptor"]["digest"], saved
    run("rm -rf -- " + layout)
    return created


def deliver(name, created, destination):
    layout = ROOT + "/restored-" + name
    loaded = cli("load --layout -i " + ROOT + "/" + name + ".tar " + layout + " --format json", "cli-load-" + name)
    assert loaded["descriptor"]["digest"] == created["descriptor"]["digest"], loaded
    inspected = cli("inspect --layout " + layout + " --format json", "cli-inspect-" + name)
    assert inspected[0]["metadata"] == created["metadata"], inspected
    pushed = cli("push --layout " + layout + " " + destination + " --delivery " + ROOT + "/" + name
                 + ".delivery.yaml --format json", "cli-push-" + name)
    assert pushed["descriptor"]["digest"] == created["descriptor"]["digest"], pushed
    assert pushed["destinations"], pushed
    return pushed


def capture_podinfo(consumer):
    upstream = config["podinfo"]
    images = {image["sourceRef"]: dict(image) for image in upstream["images"]}
    repositories = {SOURCE + "/charts/podinfo": "charts/podinfo",
                    SOURCE + "/plugins/helm": "plugins/helm", SOURCE + "/plugins/bale": "plugins/bale"}
    for reference, image in images.items():
        repository, tag = reference.rsplit(":", 1)
        mirror = "192.168.1.1:80/" + repository.split("/", 1)[1] + ":" + tag
        run("skopeo --insecure-policy copy --preserve-digests --dest-tls-verify=false oci-archive:"
            + quote(image["archive"]) + " docker://" + quote(mirror))
        inspected = read_json("skopeo inspect --tls-verify=false docker://" + quote(mirror))
        assert inspected["Digest"] == image["digest"], inspected
        repositories[repository] = "images/" + repository.split("/", 1)[1]
        image["destination"] = DESTINATION + "/podinfo/" + repositories[repository] + "@" + image["digest"]
    checksum = "d2caf247bdb0b3c7813e1b26d235245aca90794f94a133c0101cec4857d2722d"
    assert run("sha256sum " + quote(upstream["chart"])).split()[0] == checksum
    helm("push " + quote(upstream["chart"]) + " oci://" + SOURCE + "/charts --plain-http")
    recipe = {"include": [
        {"ref": SOURCE + "/charts/podinfo:6.9.4", "plugin": {
            "ref": discovery_ref,
            "input": {"schemaVersion": 1, "release": "upstream-podinfo", "namespace": "default",
                      "context": "podinfo-defaults"},
            "blobs": {"chart": {"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}}}},
        {"ref": "ghcr.io/stefanprodan/podinfo:6.9.3"}, {"ref": consumer["tagged"]}]}
    created = capture("podinfo", recipe, repositories, source_hosts=("ghcr.io", "docker.io"))
    dependencies = [dependency for resolution in created["metadata"]["dependencies"]
                    for dependency in resolution["dependencies"]]
    expected = set(images) - {"ghcr.io/stefanprodan/podinfo:6.9.3"}
    assert {dependency["ref"] for dependency in dependencies} == expected, dependencies
    for dependency in dependencies:
        assert dependency["descriptor"]["digest"] == images[dependency["ref"]]["digest"], dependency
    assert any(root["repository"] == "ghcr.io/stefanprodan/podinfo" and root["reference"] == "6.9.3"
               and root["descriptor"]["digest"] == images["ghcr.io/stefanprodan/podinfo:6.9.3"]["digest"]
               for root in created["metadata"]["roots"]), created
    chart = created["metadata"]["roots"][0]["descriptor"]["digest"]
    return {"created": created, "images": images, "chart": DESTINATION + "/podinfo/charts/podinfo@" + chart,
            "bundle": DESTINATION + "/podinfo@" + created["descriptor"]["digest"], "chartSHA256": checksum}


def podinfo_values(version):
    image = {"pullSecrets": [{"name": "bale-pull-auth"}], "pullPolicy": "Always"}
    if version == "6.9.3":
        selected = podinfo["images"]["ghcr.io/stefanprodan/podinfo:6.9.3"]
        image.update({"repository": selected["destination"].split("@")[0], "tag": "6.9.3@" + selected["digest"]})
    return {"image": image}


def podinfo_check(release, version, label):
    image = podinfo["images"]["ghcr.io/stefanprodan/podinfo:" + version]
    expected = image["destination"]
    if version == "6.9.3":
        expected = expected.replace("@", ":6.9.3@", 1)
    fullname = release if "podinfo" in release else release + "-podinfo"
    selector = quote("app.kubernetes.io/name=" + fullname)
    deployment = wait_json("kubectl get deployments -l " + selector + " -o json",
                           lambda d: len(d["items"]) == 1 and d["items"][0]["spec"]["template"]["spec"]["containers"][0]["image"] == expected)["items"][0]
    run("kubectl rollout status deployment/" + deployment["metadata"]["name"] + " --timeout=90s", 120)
    pods = read_json("kubectl get pods -l " + selector + " -o json")
    active = [p for p in pods["items"] if not p["metadata"].get("deletionTimestamp")
              and p["status"].get("phase") == "Running"
              and any(owner["kind"] == "ReplicaSet" for owner in p["metadata"].get("ownerReferences", []))]
    assert len(active) == 1, pods
    pod = active[0]
    assert pod["spec"]["containers"][0]["image"] == expected, pod
    assert pod["spec"]["containers"][0]["imagePullPolicy"] == "Always", pod
    assert pod["spec"]["imagePullSecrets"] == [{"name": "bale-pull-auth"}], pod
    assert image["digest"] in pod["status"]["containerStatuses"][0]["imageID"], pod
    reported = read_json("curl --fail --silent --show-error --max-time 10 http://" + pod["status"]["podIP"] + ":9898/version")
    assert reported["version"] == version, reported
    evidence(label, {"pod": pod, "deployment": deployment, "version": reported})
    cases.append({"case": label, "version": version, "main": expected})


def podinfo_hooks(release, label):
    hooks = helm("get hooks " + release)
    actual = set(re.findall(r'(?m)^\s+image:\s*[\"\']?([^\"\'\s]+)', hooks))
    expected = {image["destination"] for ref, image in podinfo["images"].items() if ref.startswith("docker.io/")}
    assert actual == expected, (actual, expected, hooks)
    write(ROOT + "/evidence/" + label + "-hooks.yaml", hooks)


def podinfo_resource_set(version):
    release_template = """apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: << inputs.id | quote >>
  namespace: default
spec:
  interval: 10s
  timeout: 120s
  releaseName: << inputs.id | quote >>
  chartRef:
    kind: OCIRepository
    name: << inputs.sourceName | quote >>
  test:
    enable: true
  values: VALUES
  postRenderers: << inputs.postRenderers | mustToJson >>
""".replace("VALUES", json.dumps(podinfo_values(version)))
    return {"apiVersion": "fluxcd.controlplane.io/v1", "kind": "ResourceSet",
            "metadata": {"name": "upstream-podinfo"},
            "spec": {"inputsFrom": [{"name": "upstream-podinfo"}], "wait": True,
                     "steps": [{"name": "chart", "resourcesTemplate": chart_template},
                               {"name": "release", "resourcesTemplate": release_template}]}}


def podinfo_flux_check(version, label):
    provider = reconcile_provider(name="upstream-podinfo")
    exported = provider["status"]["exportedInputs"]
    assert len(exported) == 1 and exported[0]["id"] == "upstream-podinfo", exported
    assert exported[0]["bundle"] == podinfo["bundle"], exported
    podinfo_check("upstream-podinfo", version, label)
    release = wait_json("kubectl get helmrelease upstream-podinfo -o json",
                        lambda r: ready(r) and r["status"].get("observedGeneration") == r["metadata"]["generation"])
    source = get("ocirepository", release["spec"]["chartRef"]["name"])
    assert source["spec"]["url"] == "oci://" + podinfo["chart"].split("@")[0], source
    assert source["spec"]["ref"]["digest"] == podinfo["chart"].split("@")[1], source
    assert release["spec"]["values"] == podinfo_values(version), release
    assert release["spec"]["postRenderers"] == exported[0]["postRenderers"], release
    podinfo_hooks("upstream-podinfo", label)
    evidence(label + "-flux", {"provider": provider, "release": release, "source": source})


def service_values(version, registry_secret="bale-registry-auth"):
    release = {"name": "packaged-app", "chart": fixtures["chart"]}
    if version is not None:
        release["selection"] = {"bundle": versions[version]["bundle"]}
    return {
        "image": {"repository": DESTINATION + "/service/images/bale-flux", "digest": config["serviceImageDigest"], "pullPolicy": "Always"},
        "imagePullSecrets": [{"name": "bale-pull-auth"}],
        "releases": [release],
        "bearerToken": {"existingSecret": "bale-resolver-auth"},
        "serverTLS": {"existingSecret": "bale-server-tls"},
        "registry": {"existingSecret": registry_secret, "ca": {"existingSecret": "bale-registry-ca"}},
    }


def install_service(version, action, registry_secret="bale-registry-auth"):
    write(ROOT + "/service-values.json", json.dumps(service_values(version, registry_secret)))
    output = helm(action + " bale-flux oci://" + service_chart
                  + REGISTRY_FLAGS + " --wait --timeout 120s --values " + ROOT + "/service-values.json")
    write(ROOT + "/evidence/service-" + str(version) + ".log", output)
    run("kubectl rollout status deployment/bale-flux-bale-flux --timeout=90s", 120)


def service_curl(path, authenticated=False):
    pod = service_pod()
    command = ("curl --max-time 30 --silent --show-error --cacert " + ROOT
               + "/tls/tls.crt --resolve " + SERVICE_HOST + ":8080:" + pod["status"]["podIP"])
    if authenticated:
        command += " -H " + quote("Authorization: Bearer " + TOKEN)
    return command + " -o " + ROOT + "/response.json -w '%{http_code}' " + quote("https://" + SERVICE_HOST + ":8080" + path)


def service_pod():
    pods = read_json("kubectl get pods -l app.kubernetes.io/name=bale-flux -o json")
    active = [p for p in pods["items"] if not p["metadata"].get("deletionTimestamp")
              and p["status"].get("phase") == "Running"]
    assert len(active) == 1, pods
    return active[0]


def reconcile_provider(expected_ready=True, name="bale-acceptance"):
    stamp = datetime.now(timezone.utc).isoformat()
    run("kubectl annotate resourcesetinputprovider " + quote(name) + " reconcile.fluxcd.io/requestedAt="
        + quote(stamp) + " --overwrite")
    def completed(provider):
        status = provider.get("status", {})
        condition = "True" if expected_ready else "False"
        return (status.get("lastHandledReconcileAt") == stamp
                and any(c["type"] == "Ready" and c["status"] == condition
                        for c in status.get("conditions", []))
                and (not expected_ready or bool(status.get("exportedInputs"))))
    return wait_json("kubectl get resourcesetinputprovider " + quote(name) + " -o json", completed)


def flux_check(version, label, override_main=None):
    provider = reconcile_provider()
    assert ready(provider), provider
    exported = provider["status"]["exportedInputs"]
    assert len(exported) == 1 and exported[0]["id"] == "packaged-app", exported
    assert exported[0]["bundle"] == versions[version]["bundle"], exported
    stamp = datetime.now(timezone.utc).isoformat()
    run("kubectl annotate resourceset bale-acceptance reconcile.fluxcd.io/requestedAt="
        + quote(stamp) + " --overwrite")
    app_check("packaged-app", version, label, override_main)
    release = wait_json("kubectl get helmrelease packaged-app -o json", ready)
    source = get("ocirepository", release["spec"]["chartRef"]["name"])
    assert source["spec"]["url"] == "oci://" + fixtures["chart"].split("@")[0], source
    assert source["spec"]["ref"]["digest"] == fixtures["chart"].split("@")[1], source
    assert source["spec"]["secretRef"] == {"name": "bale-pull-auth"}, source
    assert source["spec"]["certSecretRef"] == {"name": "bale-registry-ca"}, source
    assert not source["spec"].get("insecure", False), source
    assert release["spec"]["postRenderers"] == exported[0]["postRenderers"], release
    evidence(label + "-flux", {"provider": provider, "release": release, "source": source})


start_all()
run("mkdir -p " + ROOT + "/evidence " + ROOT + "/tls /root/.kube")

try:
    # easyCerts starts services while issuing their first certificates. Wait for
    # the actual node condition rather than a transient systemd failure state.
    machine.wait_until_succeeds("kubectl wait --for=condition=Ready node --all --timeout=10s", timeout=300)
    run("cp /etc/kubernetes/cluster-admin.kubeconfig /root/.kube/config")
    with subtest("capture and transfer with the source registry offline"):
        machine.wait_until_succeeds("curl -fsS http://" + SOURCE + "/v2/", timeout=30)
        run("mkdir -p " + ROOT + "/registry-tls")
        run("openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=BaleAcceptanceCA"
            + " -keyout " + ROOT + "/registry-tls/ca.key -out " + REGISTRY_CA)
        run("openssl req -newkey rsa:2048 -nodes -subj /CN=192.168.1.1 -keyout "
            + ROOT + "/registry-tls/tls.key -out " + ROOT + "/registry-tls/tls.csr")
        write(ROOT + "/registry-tls/extensions", "subjectAltName=IP:192.168.1.1\nextendedKeyUsage=serverAuth\n")
        run("openssl x509 -req -days 1 -in " + ROOT + "/registry-tls/tls.csr -CA " + REGISTRY_CA
            + " -CAkey " + ROOT + "/registry-tls/ca.key -CAcreateserial -extfile "
            + ROOT + "/registry-tls/extensions -out " + ROOT + "/registry-tls/tls.crt")
        run("htpasswd -Bbc " + ROOT + "/registry-tls/htpasswd " + REGISTRY_USER + " " + REGISTRY_PASSWORD)
        run("systemctl start " + quote(config.get("destinationUnit", "bale-destination")))
        machine.wait_until_succeeds("curl --silent --cacert " + REGISTRY_CA + " -o /dev/null -w '%{http_code}' https://"
                                    + DESTINATION + "/v2/ | grep -qx 401", timeout=30)
        for ca in ["", " --cacert /etc/ssl/certs/ca-certificates.crt"]:
            code, output = machine.execute("curl --silent --show-error --max-time 5" + ca + " https://" + DESTINATION + "/v2/ 2>&1")
            assert code == 60, output
        for credentials in ["", " -u " + REGISTRY_USER + ":wrong-password"]:
            assert run("curl --silent --cacert " + REGISTRY_CA + credentials
                       + " -o /dev/null -w '%{http_code}' https://" + DESTINATION + "/v2/").strip() == "401"
        assert run("curl --silent --cacert " + REGISTRY_CA + " -u " + REGISTRY_USER + ":" + REGISTRY_PASSWORD
                   + " -o /dev/null -w '%{http_code}' https://" + DESTINATION + "/v2/").strip() == "200"
        for path, password in [(REGISTRY_CONFIG, REGISTRY_PASSWORD), (BAD_REGISTRY_CONFIG, "wrong-password")]:
            auth = base64.b64encode((REGISTRY_USER + ":" + password).encode()).decode()
            write(path, json.dumps({"auths": {DESTINATION: {"auth": auth}}}))
        run("chmod 600 " + REGISTRY_CONFIG + " " + BAD_REGISTRY_CONFIG)
        run("kubectl create secret generic bale-pull-auth --type=kubernetes.io/dockerconfigjson --from-file=.dockerconfigjson=" + REGISTRY_CONFIG)
        run("kubectl create secret generic bale-bad-pull-auth --type=kubernetes.io/dockerconfigjson --from-file=.dockerconfigjson=" + BAD_REGISTRY_CONFIG)
        pull_values = {"imagePullSecrets": [{"name": "bale-pull-auth"}], "imagePullPolicy": "Always"}
        pull_values["child"] = {"imagePullSecrets": [{"name": "bale-pull-auth"}], "imagePullPolicy": "Always"}
        write(PULL_VALUES, json.dumps(pull_values))
        evidence("private-registry-boundary", {"trustedHTTPS": True, "anonymousStatus": 401, "wrongPasswordStatus": 401,
                                               "systemCARejected": True, "authenticatedStatus": 200})
        run("mkdir -p " + ROOT + "/docker " + PLUGINS)
        run("cp " + REGISTRY_CONFIG + " " + ROOT + "/docker/config.json")
        fixture("seed", "--workload " + quote(config["workload"]))
        fixtures = read_json("cat " + ROOT + "/seed.json")
        versions = fixtures["versions"]
        override = fixtures["override"]
        assert override["digest"] not in {entry["image_digest"] for entry in versions.values()}, fixtures
        discovery_ref = SOURCE + "/plugins/helm:acceptance"
        discovery = cli("plugins push --plain-http " + SOURCE + " -f " + quote(config["helmPlugin"])
                        + " " + discovery_ref + " --format json", "cli-publish-discovery")
        fixture("plugin-seed", "--plugin-archive " + quote(config["nativePluginArchive"]))
        consumer = read_json("cat " + ROOT + "/plugin-seed.json")
        app_recipe = {"include": [
            {"ref": SOURCE + "/charts/app:1.0.0", "plugin": {
                "ref": discovery_ref,
                "input": {"schemaVersion": 1, "release": "packaged-app", "namespace": "default",
                          "context": "acceptance"},
                "blobs": {"chart": {"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip"}}}},
            {"ref": consumer["tagged"]}, {"ref": override["ref"]}]}
        repositories = {SOURCE + "/charts/app": "charts/app", SOURCE + "/images/app": "images/app",
                        SOURCE + "/plugins/helm": "plugins/helm", SOURCE + "/plugins/bale": "plugins/bale",
                        SOURCE + "/images/override": "images/override"}
        captures = {}
        for version in ["A", "B"]:
            if version == "B":
                fixture("retag", "--version B")
            placements = dict(repositories)
            placements[SOURCE + "/images/app"] = "images/app-" + version.lower()
            captured = capture("app-" + version.lower(), app_recipe, placements)
            assert captured["metadata"]["roots"][0]["descriptor"]["digest"] == fixtures["chart"].split("@")[1], captured
            resolutions = captured["metadata"]["dependencies"]
            dependencies = [dependency for resolution in resolutions for dependency in resolution["dependencies"]]
            stable = [dependency for dependency in dependencies if dependency["ref"] == SOURCE + "/images/app:stable"]
            assert len(stable) == 1 and stable[0]["descriptor"]["digest"] == versions[version]["image_digest"], dependencies
            helper = [dependency for dependency in dependencies if dependency["ref"] == SOURCE + "/images/app:b"]
            assert len(helper) == 1 and helper[0]["descriptor"]["digest"] == versions["B"]["image_digest"], dependencies
            hook = [dependency for dependency in dependencies if dependency["ref"] == SOURCE + "/images/app:hook"]
            assert len(hook) == 1 and hook[0]["descriptor"]["digest"] == versions[version]["image_digest"], dependencies
            assert {dependency["ref"] for dependency in dependencies} == {
                SOURCE + "/images/app:stable", SOURCE + "/images/app:b", SOURCE + "/images/app:hook"}, dependencies
            assert any(root["repository"] == SOURCE + "/images/override"
                       and root["descriptor"]["digest"] == override["digest"]
                       for root in captured["metadata"]["roots"]), captured
            captures[version] = captured
        run("skopeo --insecure-policy copy --dest-tls-verify=false oci-archive:"
            + quote(config["serviceImage"]) + " docker://" + SOURCE + "/images/bale-flux:acceptance")
        fixture("service-seed", "--service-chart " + quote(config["serviceChart"]))
        seeded = read_json("cat " + ROOT + "/service-seed.json")
        service_recipe = {"include": [
            {"ref": SOURCE + "/charts/bale-flux:" + seeded["version"].replace("+", "_")},
            {"ref": SOURCE + "/images/bale-flux:acceptance"}]}
        service_capture = capture("service", service_recipe, {
            SOURCE + "/charts/bale-flux": "charts/bale-flux", SOURCE + "/images/bale-flux": "images/bale-flux"},
            expected_plugin=False)
        service_dependencies = [dependency for resolution in service_capture["metadata"]["dependencies"]
                                for dependency in resolution["dependencies"]]
        assert not service_dependencies, service_dependencies
        assert any(root["repository"] == SOURCE + "/images/bale-flux"
                   and root["reference"] == "acceptance"
                   and root["descriptor"]["digest"] == config["serviceImageDigest"]
                   for root in service_capture["metadata"]["roots"]), service_capture
        podinfo = capture_podinfo(consumer)
        run("systemctl stop " + quote(config.get("sourceUnit", "bale-source")))
        run("systemctl stop " + quote(config["upstreamUnit"]))
        registry_offline(5001)
        registry_offline(80)
        for version in ["A", "B"]:
            pushed = deliver("app-" + version.lower(), captures[version], DESTINATION + "/app:" + version.lower())
            versions[version]["bundle"] = DESTINATION + "/app@" + pushed["descriptor"]["digest"]
            versions[version]["destination_image"] = DESTINATION + "/app/images/app-" + version.lower() + "@" + versions[version]["image_digest"]
            assert any(row["destination"]["repository"] == DESTINATION + "/app/images/app-" + version.lower()
                       and row["destination"]["digest"] == versions[version]["image_digest"] for row in pushed["relocations"]), pushed
            assert any(row["source"]["repository"] == SOURCE + "/images/override"
                       and row["destination"]["repository"] == DESTINATION + "/app/images/override"
                       and row["destination"]["digest"] == override["digest"] for row in pushed["relocations"]), pushed
            assert any(tag["repository"] == DESTINATION + "/app/images/override" and tag["reference"] == "custom"
                       for tag in pushed["tags"]), pushed
            assert any(tag["repository"] == DESTINATION + "/app/plugins/bale" and tag["reference"] == "0.0.0"
                       for tag in pushed["tags"]), pushed
        override["destination_image"] = DESTINATION + "/app/images/override@" + override["digest"]
        override_values = dict(pull_values, image=override["destination_image"])
        write(ROOT + "/override-values.json", json.dumps(override_values))
        run("cp " + ROOT + "/override-values.json " + ROOT + "/evidence/")
        deliver("service", service_capture, DESTINATION + "/service:bootstrap")
        podinfo_push = deliver("podinfo", podinfo["created"], DESTINATION + "/podinfo:bundle")
        for reference, image in podinfo["images"].items():
            assert any(row["source"]["repository"] == reference.rsplit(":", 1)[0]
                       and row["destination"]["repository"] == image["destination"].split("@")[0]
                       and row["destination"]["digest"] == image["digest"]
                       for row in podinfo_push["relocations"]), (reference, podinfo_push)
        evidence("podinfo-fixtures", podinfo)
        fixtures["chart"] = DESTINATION + "/app/charts/app@" + fixtures["chart"].split("@")[1]
        service_chart = DESTINATION + "/service/charts/bale-flux@" + seeded["chart"].split("@")[1]
        assert versions["A"]["image_digest"] != versions["B"]["image_digest"], versions
        # Bootstrap the consumer from the transferred OCI package, with no Nix
        # plugin directory mounted into Helm and no source registry available.
        # Helm 4.3's OCI plugin getter uses the default Helm credential path,
        # so align it with the configured path and populate it through login.
        write(ROOT + "/registry-password", REGISTRY_PASSWORD + "\n")
        run("chmod 600 " + ROOT + "/registry-password")
        write(ROOT + "/evidence/helm-registry-login.log", helm("registry login " + DESTINATION
              + " --username " + REGISTRY_USER + " --password-stdin" + REGISTRY_FLAGS
              + " <" + ROOT + "/registry-password"))
        write(ROOT + "/evidence/helm-plugin-install.log", helm("plugin install oci://" + DESTINATION
              + "/app/plugins/bale:0.0.0 --verify=false" + REGISTRY_FLAGS))
        plugins = helm("plugin list")
        assert any(line.split() and line.split()[0] == "bale" for line in plugins.splitlines()), plugins
        write(ROOT + "/evidence/helm-plugin-list.log", plugins)
        registry_offline(5001)
        evidence("fixtures", fixtures)

    with subtest("concurrent native Helm releases, upgrades, rollback and replacement"):
        # A private CA must be configured for both attachment lookup and chart fetch.
        command = "bale --bundle " + quote(versions["A"]["bundle"]) + " template untrusted oci://" + fixtures["chart"]
        code, output = machine.execute(helm_command(command) + " 2>&1", timeout=60)
        assert code != 0 and "certificate" in output.lower(), output
        code, output = machine.execute(helm_command("pull oci://" + fixtures["chart"] + REGISTRY_FLAGS
                                       + " --destination " + ROOT, BAD_REGISTRY_CONFIG) + " 2>&1", timeout=60)
        assert code != 0 and any(word in output.lower() for word in ["401", "unauthorized", "authentication"]), output
        evidence("helm-private-registry-boundary", {"missingCARejected": True, "stockHelmWrongCredentialsRejected": True})
        commands = []
        for release, version in [("native-a", "A"), ("native-b", "B")]:
            commands.append(helm_command("bale --bundle " + quote(versions[version]["bundle"])
                            + " install " + release + " oci://" + fixtures["chart"]
                            + REGISTRY_FLAGS + " --values " + PULL_VALUES + " --wait --timeout 120s")
                            + " >" + ROOT + "/evidence/" + release + ".log 2>&1")
        run("set -e; " + commands[0] + " & first=$!; " + commands[1] + " & second=$!; wait $first; wait $second")
        app_check("native-a", "A", "helm-installed-a")
        app_check("native-b", "B", "helm-installed-b")
        helm("bale --bundle " + quote(versions["A"]["bundle"]) + " install native-override oci://"
             + fixtures["chart"] + REGISTRY_FLAGS + " --values " + ROOT + "/override-values.json --wait --timeout 120s")
        app_check("native-override", "A", "helm-manual-override", override)
        code, output = machine.execute(helm_command("bale template ambiguous oci://" + fixtures["chart"] + REGISTRY_FLAGS) + " 2>&1")
        assert code != 0 and "ambig" in output.lower(), output
        evidence("helm-ambiguous", {"status": code, "output": output})
        helm("bale --bundle " + quote(versions["B"]["bundle"]) + " upgrade native-a oci://"
             + fixtures["chart"] + REGISTRY_FLAGS + " --values " + PULL_VALUES + " --wait --timeout 120s")
        app_check("native-a", "B", "helm-upgraded-b")
        helm("rollback native-a 1 --wait --timeout 120s")
        old = app_check("native-a", "A", "helm-rollback-a")
        run("kubectl delete pod " + quote(old["metadata"]["name"]) + " --wait=true")
        replacement = app_check("native-a", "A", "helm-replacement-a")
        assert old["metadata"]["uid"] != replacement["metadata"]["uid"]
        helm("pull oci://" + fixtures["chart"] + REGISTRY_FLAGS + " --destination " + ROOT)
        run("cmp " + ROOT + "/original-chart.tgz " + ROOT + "/app@*.tgz")

    with subtest("private pulls require workload credentials even for cached image blobs"):
        expected = versions["A"]["destination_image"]
        probe = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "private-pull-probe"},
                 "spec": {"restartPolicy": "Never", "imagePullSecrets": [{"name": "bale-bad-pull-auth"}],
                          "containers": [{"name": "app", "image": expected, "imagePullPolicy": "Always"}]}}
        apply(probe)
        failed = wait_json("kubectl get pod private-pull-probe -o json", lambda p: any(
            c.get("state", {}).get("waiting", {}).get("reason") in ["ErrImagePull", "ImagePullBackOff"]
            for c in p.get("status", {}).get("containerStatuses", [])), timeout=90)
        assert not failed["status"].get("containerStatuses", [{}])[0].get("imageID"), failed
        pull_events = wait_json("kubectl get events --field-selector involvedObject.uid="
                               + failed["metadata"]["uid"] + " -o json", lambda events: any(
            any(word in event.get("message", "").lower() for word in ["401", "unauthorized", "authentication"])
            for event in events["items"]), timeout=30)
        run("kubectl delete pod private-pull-probe --wait=true")
        probe["spec"]["imagePullSecrets"] = [{"name": "bale-pull-auth"}]
        apply(probe)
        run("kubectl wait --for=condition=Ready pod/private-pull-probe --timeout=90s", 120)
        recovered = get("pod", "private-pull-probe")
        assert recovered["metadata"]["uid"] != failed["metadata"]["uid"], recovered
        assert versions["A"]["image_digest"] in recovered["status"]["containerStatuses"][0]["imageID"], recovered
        evidence("private-pull-credentials", {"failed": failed, "failureEvents": pull_events, "recovered": recovered})
        run("kubectl delete pod private-pull-probe --wait=true")

    with subtest("Helm registry credential failure leaves the release unchanged"):
        before = get("deployment", "native-a")
        before_pod = app_check("native-a", "A", "helm-before-bad-credentials")
        history = read_json(helm_command("history native-a -o json"))
        command = ("bale --bundle " + quote(versions["B"]["bundle"]) + " upgrade native-a oci://"
                   + fixtures["chart"] + REGISTRY_FLAGS + " --values " + PULL_VALUES + " --wait --timeout 120s")
        code, output = machine.execute(helm_command(command, BAD_REGISTRY_CONFIG) + " 2>&1", timeout=60)
        assert code != 0 and any(word in output.lower() for word in ["401", "unauthorized", "authentication"]), output
        assert get("deployment", "native-a")["spec"] == before["spec"]
        assert read_json(helm_command("history native-a -o json")) == history
        after_pod = app_check("native-a", "A", "helm-bad-credentials-preserved-a")
        assert after_pod["metadata"]["uid"] == before_pod["metadata"]["uid"]
        evidence("helm-bad-credentials", {"status": code, "output": output, "releaseHistoryUnchanged": True,
                                          "deploymentSpecUnchanged": True, "podUIDUnchanged": True})

    with subtest("pinned Flux controllers from Nix-provided OCI archives"):
        pinned_images = {}
        for controller in config["controllers"]:
            repository, digest = controller["digestRef"].split("@")
            pinned_images[repository.rsplit("/", 1)[1]] = controller["digestRef"]
            # The local importer registers each manifest from index.json.
            # The transfer importer instead registers the outer archive index.
            run("ctr -n k8s.io images import --local --digests --base-name " + quote(repository)
                + " " + quote(controller["archive"]))
            write(ROOT + "/evidence/" + repository.rsplit("/", 1)[1] + "-images.log",
                  run("ctr -n k8s.io images list"))
            run("ctr -n k8s.io images list | awk -v ref=" + quote(controller["digestRef"])
                + " -v digest=" + quote(digest)
                + " '$1 == ref && $3 == digest {found=1} END {exit !found}'")
        pinned_controllers = set()
        for manifest in [config["controllerManifest"], config["operatorManifest"]]:
            # kubectl create prints one JSON object per YAML document rather
            # than a List. Decode that stream before changing any deployments.
            stream = run("kubectl create --dry-run=client --validate=false -f "
                         + quote(manifest) + " -o json").strip()
            resources = {"apiVersion": "v1", "kind": "List", "items": []}
            decoder = json.JSONDecoder()
            while stream:
                resource, end = decoder.raw_decode(stream)
                if resource["kind"] == "List":
                    resources["items"].extend(resource["items"])
                else:
                    resources["items"].append(resource)
                stream = stream[end:].strip()
            for resource in resources["items"]:
                if resource["kind"] != "Deployment":
                    continue
                name = resource["metadata"]["name"]
                assert name in pinned_images, name
                containers = resource["spec"]["template"]["spec"]["containers"]
                assert len(containers) == 1, resource
                containers[0]["image"] = pinned_images[name]
                containers[0]["imagePullPolicy"] = "Never"
                pinned_controllers.add(name)
            write(ROOT + "/controllers.json", json.dumps(resources))
            run("kubectl apply --server-side --field-manager=bale-acceptance -f "
                + ROOT + "/controllers.json")
        assert pinned_controllers == set(pinned_images), pinned_controllers
        for controller in ["source-controller", "helm-controller", "flux-operator"]:
            run("kubectl -n flux-system rollout status deployment/" + controller + " --timeout=90s", 120)

    with subtest("bootstrap the packaged TLS service from its destination bundle"):
        run("openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=" + SERVICE_HOST
            + " -addext subjectAltName=DNS:" + SERVICE_HOST + ",DNS:" + SERVICE_HOST + ".cluster.local"
            + " -keyout " + ROOT + "/tls/tls.key -out " + ROOT + "/tls/tls.crt")
        run("kubectl create secret tls bale-server-tls --cert=" + ROOT + "/tls/tls.crt --key=" + ROOT + "/tls/tls.key")
        run("kubectl create secret generic bale-resolver-auth --from-literal=token=" + TOKEN)
        run("kubectl create secret generic bale-resolver-ca --from-file=ca.crt=" + ROOT + "/tls/tls.crt")
        run("kubectl create secret generic bale-registry-auth --from-file=config.json=" + ROOT + "/registry-auth.json")
        run("kubectl create secret generic bale-bad-registry-auth --from-file=config.json=" + BAD_REGISTRY_CONFIG)
        run("kubectl create secret generic bale-registry-ca --from-file=ca.crt=" + REGISTRY_CA)
        install_service("A", "install")
        pod = service_pod()
        expected = DESTINATION + "/service/images/bale-flux@" + config["serviceImageDigest"]
        container = pod["spec"]["containers"][0]
        assert container["image"] == expected, pod
        assert container["imagePullPolicy"] == "Always", pod
        assert pod["spec"]["imagePullSecrets"] == [{"name": "bale-pull-auth"}], pod
        assert config["serviceImageDigest"] in pod["status"]["containerStatuses"][0]["imageID"], pod
        assert pod["spec"]["securityContext"]["runAsNonRoot"] is True, pod
        assert pod["spec"]["securityContext"]["runAsUser"] == 65532, pod
        assert container["securityContext"]["readOnlyRootFilesystem"] is True, pod
        for probe in ["readinessProbe", "livenessProbe"]:
            assert container[probe]["httpGet"]["path"] == "/healthz", pod
            assert container[probe]["httpGet"]["scheme"] == "HTTPS", pod
        cid = pod["status"]["containerStatuses"][0]["containerID"].split("://")[1]
        runtime = read_json("ctr -n k8s.io containers info " + quote(cid))
        user = runtime["Spec"]["process"]["user"]
        assert user["uid"] == 65532 and user["gid"] == 65532, runtime
        assert runtime["Spec"]["root"]["readonly"] is True, runtime
        assert run(service_curl("/inputs/packaged-app")).strip() == "401"
        assert run(service_curl("/inputs/packaged-app", True)).strip() == "200"
        response = read_json("cat " + ROOT + "/response.json")
        assert response["inputs"][0]["bundle"] == versions["A"]["bundle"], response
        evidence("packaged-service", {"pod": pod, "runtime": runtime, "response": response})
        helm("pull oci://" + service_chart + REGISTRY_FLAGS + " --destination " + ROOT)
        run("cmp " + quote(config["serviceChart"]) + " " + ROOT + "/bale-flux@*.tgz")

    with subtest("Flux Operator consumes attachments and reconciles A to B to A"):
        apply({"apiVersion": "fluxcd.controlplane.io/v1", "kind": "ResourceSetInputProvider",
               "metadata": {"name": "bale-acceptance", "annotations": {"fluxcd.controlplane.io/reconcileEvery": "10s"}},
               "spec": {"type": "ExternalService", "url": "https://" + SERVICE_HOST + ":8080/inputs/packaged-app",
                        "secretRef": {"name": "bale-resolver-auth"}, "certSecretRef": {"name": "bale-resolver-ca"}}})
        chart_template = """apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: << inputs.sourceName | quote >>
  namespace: default
spec:
  interval: 10s
  secretRef:
    name: bale-pull-auth
  certSecretRef:
    name: bale-registry-ca
  url: << inputs.chartRepository | quote >>
  ref:
    digest: << inputs.chartDigest | quote >>
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
"""
        release_template = """apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: << inputs.id | quote >>
  namespace: default
spec:
  interval: 10s
  timeout: 120s
  releaseName: << inputs.id | quote >>
  chartRef:
    kind: OCIRepository
    name: << inputs.sourceName | quote >>
  values:
    imagePullSecrets:
      - name: bale-pull-auth
    imagePullPolicy: Always
    child:
      imagePullSecrets:
        - name: bale-pull-auth
      imagePullPolicy: Always
  postRenderers: << inputs.postRenderers | mustToJson >>
"""
        apply({"apiVersion": "fluxcd.controlplane.io/v1", "kind": "ResourceSet",
               "metadata": {"name": "bale-acceptance"},
               "spec": {"inputsFrom": [{"name": "bale-acceptance"}], "wait": True,
                        "steps": [{"name": "chart", "resourcesTemplate": chart_template},
                                  {"name": "release", "resourcesTemplate": release_template}]}})
        flux_check("A", "flux-installed-a")
        install_service("B", "upgrade")
        flux_check("B", "flux-upgraded-b")
        install_service("A", "upgrade")
        flux_check("A", "flux-reverted-a")

    with subtest("manually transferred override uses deployment values only"):
        resource_set = get("resourceset", "bale-acceptance")
        original_steps = json.loads(json.dumps(resource_set["spec"]["steps"]))
        override_template = release_template.replace("  values:\n", "  values:\n    image: " + json.dumps(override["destination_image"]) + "\n", 1)
        resource_set["spec"]["steps"][1]["resourcesTemplate"] = override_template
        apply({"apiVersion": resource_set["apiVersion"], "kind": "ResourceSet",
               "metadata": {"name": "bale-acceptance"}, "spec": resource_set["spec"]})
        flux_check("A", "flux-manual-override", override)
        overridden = get("helmrelease", "packaged-app")
        assert overridden["spec"]["values"]["image"] == override["destination_image"], overridden
        evidence("manual-override-release", overridden)
        resource_set["spec"]["steps"] = original_steps
        apply({"apiVersion": resource_set["apiVersion"], "kind": "ResourceSet",
               "metadata": {"name": "bale-acceptance"}, "spec": resource_set["spec"]})
        flux_check("A", "flux-manual-override-restored-a")

    with subtest("unchanged upstream Podinfo through native Helm and real test hooks"):
        registry_offline(5001)
        registry_offline(80)
        # Upstream test Pods do not expose image.pullSecrets. The ordinary
        # namespace ServiceAccount supplies their destination credentials.
        run("kubectl patch serviceaccount default --type=merge -p "
            + quote(json.dumps({"imagePullSecrets": [{"name": "bale-pull-auth"}]})))
        for version in ["6.9.4", "6.9.3"]:
            path = ROOT + "/podinfo-" + version + ".values.json"
            write(path, json.dumps(podinfo_values(version)))
            run("cp " + path + " " + ROOT + "/evidence/")
        helm("bale --bundle " + quote(podinfo["bundle"]) + " install upstream-native oci://"
             + podinfo["chart"] + REGISTRY_FLAGS + " --values " + ROOT + "/podinfo-6.9.4.values.json --wait --timeout 120s")
        podinfo_check("upstream-native", "6.9.4", "podinfo-helm-default")
        podinfo_hooks("upstream-native", "podinfo-helm-default")
        write(ROOT + "/evidence/podinfo-helm-test.log", helm("test upstream-native --logs --timeout 90s"))
        helm("bale --bundle " + quote(podinfo["bundle"]) + " upgrade upstream-native oci://"
             + podinfo["chart"] + REGISTRY_FLAGS + " --values " + ROOT + "/podinfo-6.9.3.values.json --wait --timeout 120s")
        podinfo_check("upstream-native", "6.9.3", "podinfo-helm-custom-upgrade")
        podinfo_hooks("upstream-native", "podinfo-helm-custom-upgrade")
        helm("rollback upstream-native 1 --wait --timeout 120s")
        podinfo_check("upstream-native", "6.9.4", "podinfo-helm-rollback")
        helm("pull oci://" + podinfo["chart"] + REGISTRY_FLAGS + " --destination " + ROOT)
        run("cmp " + quote(config["podinfo"]["chart"]) + " " + ROOT + "/podinfo@*.tgz")

    with subtest("unchanged upstream Podinfo through Flux with deployment-only overrides"):
        values = service_values("A")
        values["releases"].append({"name": "upstream-podinfo", "chart": podinfo["chart"],
                                   "selection": {"bundle": podinfo["bundle"]}})
        write(ROOT + "/service-values.json", json.dumps(values))
        helm("upgrade bale-flux oci://" + service_chart + REGISTRY_FLAGS
             + " --wait --timeout 120s --values " + ROOT + "/service-values.json")
        apply({"apiVersion": "fluxcd.controlplane.io/v1", "kind": "ResourceSetInputProvider",
               "metadata": {"name": "upstream-podinfo", "annotations": {"fluxcd.controlplane.io/reconcileEvery": "10s"}},
               "spec": {"type": "ExternalService", "url": "https://" + SERVICE_HOST + ":8080/inputs/upstream-podinfo",
                        "secretRef": {"name": "bale-resolver-auth"}, "certSecretRef": {"name": "bale-resolver-ca"}}})
        for version, label in [("6.9.4", "podinfo-flux-default"), ("6.9.3", "podinfo-flux-custom-upgrade"),
                               ("6.9.4", "podinfo-flux-restored")]:
            apply(podinfo_resource_set(version))
            podinfo_flux_check(version, label)
        install_service("A", "upgrade")

    with subtest("Flux private source rejects bad credentials and recovers with its pull Secret"):
        probe = {"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "OCIRepository",
                 "metadata": {"name": "private-source-probe"},
                 "spec": {"interval": "10s", "url": "oci://" + fixtures["chart"].split("@")[0],
                          "ref": {"digest": fixtures["chart"].split("@")[1]},
                          "secretRef": {"name": "bale-bad-pull-auth"},
                          "certSecretRef": {"name": "bale-registry-ca"},
                          "layerSelector": {"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
                                            "operation": "copy"}}}
        apply(probe)
        failed = wait_json("kubectl get ocirepository private-source-probe -o json", lambda r: any(
            c["type"] == "Ready" and c["status"] == "False"
            and any(word in c.get("message", "").lower() for word in ["401", "unauthorized", "authentication"])
            for c in r.get("status", {}).get("conditions", [])))
        assert not failed.get("status", {}).get("artifact"), failed
        probe["spec"]["secretRef"]["name"] = "bale-pull-auth"
        apply(probe)
        recovered = wait_json("kubectl get ocirepository private-source-probe -o json", lambda r:
                              ready(r) and bool(r.get("status", {}).get("artifact")))
        assert fixtures["chart"].split("@")[1] in recovered["status"]["artifact"]["revision"], recovered
        evidence("flux-source-credentials", {"failed": failed, "recovered": recovered})
        run("kubectl delete ocirepository private-source-probe")

    with subtest("resolver registry credential failure preserves the Flux release and recovers"):
        provider_before = get("resourcesetinputprovider", "bale-acceptance")
        before = provider_before["status"]["exportedInputs"]
        release_before = get("helmrelease", "packaged-app")["spec"]
        pod_before = app_check("packaged-app", "A", "flux-before-bad-registry-credentials")
        install_service("A", "upgrade", "bale-bad-registry-auth")
        assert run(service_curl("/inputs/packaged-app", True)).strip() == "502"
        provider = reconcile_provider(expected_ready=False)
        assert provider["status"]["exportedInputs"] == before, provider
        assert get("helmrelease", "packaged-app")["spec"] == release_before
        pod_after = app_check("packaged-app", "A", "flux-bad-registry-credentials-preserved-a")
        assert pod_before["metadata"]["uid"] == pod_after["metadata"]["uid"]
        evidence("resolver-bad-registry-credentials", {"provider": provider, "releaseSpecUnchanged": True,
                                                      "podUIDUnchanged": True})
        install_service("A", "upgrade")
        flux_check("A", "flux-registry-credentials-recovered-a")

    with subtest("ambiguous attachments preserve previous Flux input and release"):
        before = get("resourcesetinputprovider", "bale-acceptance")["status"]["exportedInputs"]
        release_before = get("helmrelease", "packaged-app")["spec"]
        pod_before = app_check("packaged-app", "A", "flux-before-ambiguity")
        install_service(None, "upgrade")
        assert run(service_curl("/inputs/packaged-app", True)).strip() == "502"
        provider = reconcile_provider(expected_ready=False)
        assert not ready(provider), provider
        assert provider["status"]["exportedInputs"] == before, provider
        assert get("helmrelease", "packaged-app")["spec"] == release_before
        pod_after = app_check("packaged-app", "A", "flux-ambiguous-preserved-a")
        assert pod_after["metadata"]["uid"] == pod_before["metadata"]["uid"]
        evidence("flux-ambiguous", provider)

    with subtest("private service health survives destination registry outage"):
        unit = quote(config.get("destinationUnit", "bale-destination"))
        run("systemctl stop " + unit)
        try:
            registry_offline(5002)
            assert run(service_curl("/healthz")).strip() == "200"
            assert run(service_curl("/inputs/packaged-app")).strip() == "401"
        finally:
            run("systemctl start " + unit)
        registry_offline(5001)
        evidence("summary", {"status": "passed", "sourceOffline": True, "chartBytesPreserved": True,
                             "healthWithoutRegistry": True, "registryHTTPS": True, "registryPrivateCA": True,
                             "registryAuthentication": True, "cachedPullRequiresCredentials": True,
                             "helmCredentialFailurePreservesRelease": True, "fluxSourceCredentialRecovery": True,
                             "resolverCredentialFailurePreservesRelease": True, "publicCLILifecycle": True,
                             "ociDiscoveryPlugin": True, "bundledSubchartHookDiscovered": True,
                             "consumerInstalledFromDestination": True, "explicitManualImageRoot": True,
                             "discoveryUsesChartDefaults": True, "manualOverrideDeploymentOnly": True,
                             "upstreamPodinfo": True, "upstreamImagesLinuxAMD64": True,
                             "upstreamMirrorOffline": True, "upstreamChartBytesPreserved": True,
                             "upstreamHelmTestHooks": True, "upstreamFluxTestsEnabled": True, "cases": cases})
        print(json.dumps({"status": "passed", "cases": len(cases), "sourceOffline": True}))
finally:
    # Preserve useful diagnostics on failures as well as successful assertions.
    machine.execute("kubectl get pods,deployments,jobs -A -o json > " + ROOT + "/evidence/workloads.json")
    machine.execute("kubectl get events -A --sort-by=.metadata.creationTimestamp > " + ROOT + "/evidence/events.log")
    machine.execute("kubectl logs -n flux-system deployment/flux-operator > " + ROOT + "/evidence/operator.log 2>&1")
    machine.copy_from_machine(ROOT + "/evidence")
