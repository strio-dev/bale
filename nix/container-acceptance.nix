{
  pkgs,
  container,
  registry-acceptance,
}:
let
  inherit (pkgs) lib;
  imageName = "localhost/bale-container-acceptance:nix";
  work = "/home/bale/work";
  archive = pkgs.runCommand "bale-container-acceptance.tar" { } ''
    ${pkgs.skopeo}/bin/skopeo --tmpdir "$TMPDIR" --insecure-policy copy \
      oci:${container}:${container.imageTag} docker-archive:$out:${imageName}
  '';
  wrapper =
    arbitraryUser:
    pkgs.writeShellScript "bale-container-${if arbitraryUser then "uid-1000" else "default-user"}" ''
      # CLI transport tests deliberately replace the entire environment. Capture
      # their inputs before restoring the rootless Podman client's environment.
      containerEnv=()
      for name in DOCKER_CONFIG XDG_CACHE_HOME SSL_CERT_FILE SSL_CERT_DIR PATH; do
        if [[ -v "$name" ]]; then
          containerEnv+=(--env "$name=''${!name}")
        fi
      done
      exec ${pkgs.coreutils}/bin/env \
        HOME=/home/bale USER=bale LOGNAME=bale \
        XDG_RUNTIME_DIR=/run/user/1000 \
        XDG_CONFIG_HOME=/home/bale/.config \
        XDG_DATA_HOME=/home/bale/.local/share \
        XDG_CACHE_HOME=/home/bale/.cache \
        PATH=${
          lib.makeBinPath [
            pkgs.podman
            pkgs.coreutils
          ]
        } \
        ${pkgs.podman}/bin/podman run --rm --pull=never --network=host --log-driver=none \
        ${
          if arbitraryUser then
            "--userns=keep-id --user=1000:1000"
          else
            "--userns=keep-id:uid=65532,gid=65532"
        } \
        --volume ${work}:${work}:rw --workdir ${work} \
        "''${containerEnv[@]}" ${imageName} "$@"
    '';
  runners =
    map
      (arbitraryUser: {
        name = if arbitraryUser then "uid-1000" else "default-user";
        package = registry-acceptance.override { bale-cli = wrapper arbitraryUser; };
      })
      [
        false
        true
      ];
  runTests = pkgs.writeShellApplication {
    name = "bale-container-acceptance";
    runtimeInputs = [
      pkgs.podman
      pkgs.gnugrep
      pkgs.coreutils
    ];
    text = ''
      export HOME=/home/bale XDG_RUNTIME_DIR=/run/user/1000
      mkdir -p ${work}/tmp
      export TMPDIR=${work}/tmp
      podman load --input ${archive}
      ${lib.concatMapStringsSep "\n" (runner: ''
        ${runner.package}/bin/bale-registry-acceptance \
          '-test.run=^(TestOfflineAcceptance|TestRegistryTransportAcceptance)$' \
          2>&1 | tee ${work}/${runner.name}.log
        if grep -q -- '--- SKIP:' ${work}/${runner.name}.log; then
          echo 'Container acceptance must not skip selected tests' >&2
          exit 1
        fi
        for selected in TestOfflineAcceptance TestRegistryTransportAcceptance; do
          grep -E -- "^--- PASS: $selected " ${work}/${runner.name}.log
          for backend in distribution zot; do
            grep -E -- "^[[:space:]]+--- PASS: $selected/$backend " ${work}/${runner.name}.log
          done
        done
      '') runners}
    '';
  };
in
pkgs.testers.runNixOSTest {
  name = "bale-container-acceptance";
  requiredFeatures.kvm = true;
  qemu.forceAccel = true;
  nodes.machine = {
    virtualisation = {
      memorySize = 4096;
      cores = 4;
      diskSize = 16384;
      podman.enable = true;
      containers.storage.settings.storage = {
        graphroot = lib.mkForce "";
        runroot = lib.mkForce "";
      };
      containers.containersConf.settings.engine.helper_binaries_dir = [
        "${pkgs.netavark}/bin"
        "${pkgs.aardvark-dns}/bin"
      ];
      additionalPaths = [ runTests ];
    };
    systemd.services."user@".serviceConfig.Delegate = "pids memory cpu cpuset";
    users.users.bale = {
      isNormalUser = true;
      uid = 1000;
      autoSubUidGidRange = true;
      linger = true;
    };
    system.stateVersion = "26.05";
  };
  testScript = ''
    machine.wait_for_unit("default.target")
    machine.wait_for_unit("user@1000.service")
    try:
        machine.succeed("su - bale -c '${runTests}/bin/bale-container-acceptance'", timeout=10 * 60)
    finally:
        machine.execute("su - bale -c '${pkgs.podman}/bin/podman info' >&2")
        machine.copy_from_machine("${work}")
  '';
}
