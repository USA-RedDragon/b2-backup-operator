package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const backupImageFlag = "--backup-image"

// operatorDeploymentLabels select the operator's Deployment as config/manager
// labels it.
var operatorDeploymentLabels = client.MatchingLabels{
	"app.kubernetes.io/name": "b2-backup-operator",
	"control-plane":          "controller-manager",
}

// operatorImage returns the backup image the operator runs, from the
// --backup-image argument of its Deployment, and says where it came from. When
// no Deployment can be read, or none sets the flag, it is backup.DefaultImage.
func operatorImage(ctx context.Context, c client.Reader) (image, source string) {
	image, source, err := operatorBackupImage(ctx, c)
	if err != nil {
		return backup.DefaultImage, "the built-in default; " + err.Error()
	}
	return image, source
}

func operatorBackupImage(ctx context.Context, c client.Reader) (image, source string, err error) {
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, operatorDeploymentLabels); err != nil {
		return "", "", fmt.Errorf("could not read the operator Deployment: %w", err)
	}
	found := map[string][]string{}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		for _, ctr := range d.Spec.Template.Spec.Containers {
			if ctr.Name != "manager" && !slices.Contains(ctr.Command, "/manager") {
				continue
			}
			img := flagValue(append(slices.Clone(ctr.Command), ctr.Args...), backupImageFlag)
			if img == "" {
				img = backup.DefaultImage
			}
			found[img] = append(found[img], "deployment/"+d.Namespace+"/"+d.Name)
		}
	}
	switch len(found) {
	case 0:
		return "", "", fmt.Errorf("no operator Deployment labelled app.kubernetes.io/name=b2-backup-operator found")
	case 1:
		for img, where := range found {
			return img, "the " + backupImageFlag + " of " + strings.Join(where, ", "), nil
		}
	}
	var all []string
	for img, where := range found {
		all = append(all, img+" in "+strings.Join(where, ", "))
	}
	slices.Sort(all)
	return "", "", fmt.Errorf("operator Deployments disagree on %s: %s; pass the image", backupImageFlag,
		strings.Join(all, "; "))
}

// flagValue returns the value of a Go flag in argv, given as --name=value,
// -name=value, --name value or -name value.
func flagValue(argv []string, name string) string {
	bare := strings.TrimLeft(name, "-")
	value := ""
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		key, v, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if key != bare {
			continue
		}
		if !hasValue && i+1 < len(argv) {
			i++
			v = argv[i]
		}
		value = v
	}
	return value
}
