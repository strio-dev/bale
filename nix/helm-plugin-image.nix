{
  callPackage,
  helm-plugin-archive,
}:
(callPackage ./oci-artifact.nix { }) {
  name = "bale-helm-discovery-oci-${helm-plugin-archive.pluginVersion}";
  artifactType = "application/vnd.bale.plugin.v1+json";
  layers = [
    {
      file = "${helm-plugin-archive}/${helm-plugin-archive.archiveName}";
      title = helm-plugin-archive.archiveName;
      mediaType = "application/vnd.oci.image.layer.v1.tar+gzip";
    }
  ];
}
