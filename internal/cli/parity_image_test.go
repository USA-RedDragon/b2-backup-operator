package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
)

const parityHelmRelease = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  namespace: prod
spec:
  values:
    controllers:
      restic:
        type: cronjob
        cronjob:
          schedule: "36 * * * *"
          timeZone: America/Chicago
        containers:
          restic:
            image:
              repository: ghcr.io/gabe565/restic
              tag: 0.19.1
            envFrom:
              - secretRef:
                  name: restic-b2
              - secretRef:
                  name: restic-envs
            env:
              TZ: America/Chicago
              RESTIC_HOST:
                fieldRef:
                  fieldPath: metadata.namespace
            command:
              - sh
              - -c
              - |
                exec > >(ts '%H:%M:%S') 2>&1
                set -eu
                restic backup --tag=db --stdin-from-command -- dumpdb cnpg
                restic cache --cleanup
`

const parityGenerated = `apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: web-files
  namespace: prod
spec:
  repositoryRef:
    name: store
  schedule: "@hourly"
  timeZone: America/Chicago
  sources:
    - type: cnpg
`

const flagBackupImage = "--backup-image"

func TestParityComparesAgainstTheOperatorsImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		args  []string
		objs  []client.Object
		image string
	}{
		{name: "--backup-image", args: []string{flagBackupImage, clusterImage}, image: clusterImage},
		{name: "the operator Deployment", objs: []client.Object{operatorDeployment(flagBackupImage + "=" + clusterImage)},
			image: clusterImage},
		{name: "--backup-image over the Deployment", args: []string{flagBackupImage, explicitImage},
			objs: []client.Object{operatorDeployment(flagBackupImage + "=" + clusterImage)}, image: explicitImage},
		{name: "no Deployment", image: backup.DefaultImage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			hr, gen := filepath.Join(dir, "helmrelease.yaml"), filepath.Join(dir, "generated.yaml")
			for path, body := range map[string]string{hr: parityHelmRelease, gen: parityGenerated} {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out, errOut bytes.Buffer
			f := cli.NewTestFactoryIn(bareNS, bareClient(t, tt.objs...), &out, &errOut, nil)
			_ = cli.Execute(f, append([]string{"parity", gen, hr}, tt.args...)...)
			report := out.String()
			if !strings.Contains(report, "-> "+tt.image+",") && !strings.Contains(report, "generated "+tt.image) {
				t.Errorf("parity did not compare against %s:\n%s", tt.image, report)
			}
			if !strings.Contains(errOut.String(), "operator backup image: "+tt.image) {
				t.Errorf("stderr %q does not name %s", errOut.String(), tt.image)
			}
		})
	}
}
