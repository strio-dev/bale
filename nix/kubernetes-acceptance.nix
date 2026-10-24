{
  pkgs,
  bale,
  helm-plugin-archive,
  helm-consumer-archive,
  flux-service-image,
  flux-service-chart,
}:
let
  inherit (pkgs) lib;
  inputs = import ./kubernetes-inputs.nix { inherit pkgs; };
  fixtures = bale.overrideAttrs {
    pname = "bale-kubernetes-fixtures";
    subPackages = [
      "tests/kubernetes/fixture"
      "tests/kubernetes/workload"
    ];
    ldflags = [
      "-s"
      "-w"
    ];
    checkPhase = ''
      go test ./tests/kubernetes/fixture ./tests/kubernetes/workload
    '';
  };
  serviceArchive =
    pkgs.runCommand "bale-flux-acceptance.oci.tar" { nativeBuildInputs = [ pkgs.skopeo ]; }
      ''
        skopeo --tmpdir "$TMPDIR" --insecure-policy copy --preserve-digests \
              oci:${flux-service-image}:${flux-service-image.imageTag} oci-archive:$out
      '';
  guestConfig = {
    bale = "${bale}/bin/bale";
    helmPlugin = "${helm-plugin-archive}/${helm-plugin-archive.archiveName}";
    nativePluginArchive = "${helm-consumer-archive}/${helm-consumer-archive.archiveName}";
    fixture = "${fixtures}/bin/fixture";
    workload = "${fixtures}/bin/workload";
    serviceImage = "/etc/bale-acceptance/service-image.oci.tar";
    serviceChart = "/etc/bale-acceptance/service-chart.tgz";
    controllers = lib.mapAttrsToList (name: image: {
      archive = "/etc/bale-acceptance/${name}.oci.tar";
      sourceRef = image.tagReference;
      digestRef = image.reference;
    }) inputs.images;
    podinfo = {
      chart = "/etc/bale-acceptance/podinfo-chart.tgz";
      images = lib.imap0 (index: image: {
        archive = "/etc/bale-acceptance/podinfo-${toString index}.oci.tar";
        sourceRef = image.tagReference;
        digestRef = image.reference;
        inherit (image) digest version;
      }) inputs.podinfo.images;
    };
    controllerManifest = "/etc/bale-acceptance/flux-install.yaml";
    operatorManifest = "/etc/bale-acceptance/operator-install.yaml";
    sourceUnit = "bale-source";
    destinationUnit = "bale-destination";
    upstreamUnit = "bale-upstream";
  };
  registry = name: port: {
    # The test creates the destination's certificates and password file before
    # starting it, rather than storing generated private keys in the Nix store.
    wantedBy = lib.optionals (name != "destination") [ "multi-user.target" ];
    serviceConfig = {
      StateDirectory = "bale-registry-${name}";
      ExecStart = "${pkgs.distribution}/bin/registry serve ${pkgs.writeText "${name}-registry.yaml" ''
        version: 0.1
        log:
          level: warn
        storage:
          filesystem:
            rootdirectory: /var/lib/bale-registry-${name}
        http:
          addr: 0.0.0.0:${toString port}
        ${lib.optionalString (name == "destination") ''
            tls:
              certificate: /var/lib/bale-acceptance/registry-tls/tls.crt
              key: /var/lib/bale-acceptance/registry-tls/tls.key
          auth:
            htpasswd:
              realm: bale-acceptance
              path: /var/lib/bale-acceptance/registry-tls/htpasswd
        ''}
      ''}";
    };
  };
  test = pkgs.testers.runNixOSTest {
    name = "bale-kubernetes-acceptance";
    nodes.machine = {
      virtualisation = {
        memorySize = 4096;
        cores = 4;
        diskSize = 16384;
      };
      networking = {
        firewall.enable = false;
        extraHosts = "192.168.1.1 machine.test ghcr.io docker.io registry-1.docker.io index.docker.io";
        primaryIPAddress = "192.168.1.1";
      };
      services.flannel.iface = "eth1";
      services.kubernetes = {
        easyCerts = true;
        roles = [
          "master"
          "node"
        ];
        masterAddress = "machine.test";
        proxy.hostname = "machine.test";
        apiserver = {
          securePort = 443;
          advertiseAddress = "192.168.1.1";
        };
        featureGates.AllowParsingUserUIDFromCertAuth = true;
        kubelet.unschedulable = false;
      };
      virtualisation.containerd.settings.plugins = {
        "io.containerd.grpc.v1.cri".registry.config_path = "/etc/containerd/certs.d";
        "io.containerd.cri.v1.images".registry.config_path = "/etc/containerd/certs.d";
      };
      systemd.services = {
        bale-source = registry "source" 5001;
        bale-upstream = registry "upstream" 80;
        bale-destination = registry "destination" 5002;
      };
      environment = {
        variables = {
          KUBECONFIG = "/etc/kubernetes/cluster-admin.kubeconfig";
        };
        etc =
          lib.mapAttrs' (
            name: image: lib.nameValuePair "bale-acceptance/${name}.oci.tar" { source = image.archive; }
          ) inputs.images
          // lib.listToAttrs (
            lib.imap0 (
              index: image:
              lib.nameValuePair "bale-acceptance/podinfo-${toString index}.oci.tar" {
                source = image.archive;
              }
            ) inputs.podinfo.images
          )
          // {
            "bale-acceptance/podinfo-chart.tgz".source = inputs.podinfo.chart;
            "bale-acceptance/service-image.oci.tar".source = serviceArchive;
            "bale-acceptance/service-chart.tgz".source =
              "${flux-service-chart}/bale-flux-${flux-service-chart.chartVersion}.tgz";
            "bale-acceptance/flux-install.yaml".source = inputs.fluxInstall;
            "bale-acceptance/operator-install.yaml".source = inputs.operatorInstall;
            "containerd/certs.d/192.168.1.1:5001/hosts.toml".text = ''
              server = "http://192.168.1.1:5001"
              [host."http://192.168.1.1:5001"]
                capabilities = ["pull", "resolve", "push"]
            '';
            "containerd/certs.d/192.168.1.1:5002/hosts.toml".text = ''
              server = "https://192.168.1.1:5002"
              [host."https://192.168.1.1:5002"]
                capabilities = ["pull", "resolve", "push"]
                ca = "/var/lib/bale-acceptance/registry-tls/ca.crt"
            '';
          };
        systemPackages = [
          fixtures
          pkgs.kubernetes
          pkgs.kubernetes-helm
          pkgs.containerd
          pkgs.skopeo
          pkgs.jq
          pkgs.openssl
          pkgs.apacheHttpd
          pkgs.curl
        ];
      };
      system.stateVersion = "26.05";
    };
    testScript = ''
      import json
      config = json.loads(${builtins.toJSON (builtins.toJSON guestConfig)})
      config["serviceImageDigest"] = json.loads(open("${flux-service-image}/index.json").read())["manifests"][0]["digest"]
      ${builtins.readFile ../tests/kubernetes/test.py}
    '';
  };
  runner = pkgs.writeShellApplication {
    name = "bale-kubernetes-acceptance";
    runtimeInputs = [ pkgs.coreutils ];
    text = ''
      output=""
      if [[ $# -gt 0 ]]; then
        case "$1" in
          --help|-h)
            echo 'Usage: bale-kubernetes-acceptance [--output DIRECTORY]'
            echo 'Run an isolated NixOS Kubernetes VM; optionally retain logs and evidence.'
            exit 0
            ;;
          --output)
            [[ $# -eq 2 && -n "$2" ]] || { echo '--output requires a directory' >&2; exit 2; }
            output="$2"
            ;;
          *) echo 'Expected --output DIRECTORY or --help' >&2; exit 2 ;;
        esac
      fi
      [[ -r /dev/kvm && -w /dev/kvm ]] || { echo 'Kubernetes acceptance requires access to /dev/kvm on Linux x86_64' >&2; exit 1; }
      runtime=$(mktemp -d -t bale-kubernetes-acceptance.XXXXXXXX)
      trap 'rm -rf -- "$runtime"' EXIT
      export XDG_RUNTIME_DIR="$runtime"
      export TMPDIR="$runtime/tmp"
      mkdir -p -- "$TMPDIR"
      if [[ -z "$output" ]]; then
        output="$runtime/output"
        mkdir -p -- "$output"
      else
        if [[ -d "$output" && -n "$(ls -A -- "$output")" ]]; then
          echo 'The output directory must be empty' >&2
          exit 2
        fi
        mkdir -p -- "$output"
        output=$(realpath -- "$output")
      fi
      ${test.driver}/bin/nixos-test-driver -o "$output" --junit-xml junit.xml
    '';
  };
in
{
  inherit test runner;
}
