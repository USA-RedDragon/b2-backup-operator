package tui

import (
	"context"
	"fmt"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// action is something the TUI can do, gated by RBAC.
type action string

const (
	groupB2Backup = "b2backup.mcswain.dev"
	groupBatch    = "batch"
	verbCreate    = "create"
)

const (
	actBackup   action = "backup"
	actSuspend  action = "suspend"
	actCancel   action = "cancel"
	actLogs     action = "logs"
	actPod      action = "pod"
	actNewClaim action = "newclaim"
)

type grant struct {
	group, resource, subresource, verb string
}

// required are the grants each action needs, as b2bk's subcommands use them.
var required = map[action][]grant{
	actBackup:  {{group: groupB2Backup, resource: "scheduledbackups", verb: "patch"}},
	actSuspend: {{group: groupB2Backup, resource: "scheduledbackups", verb: "patch"}},
	actCancel:  {{group: groupBatch, resource: "jobs", verb: "delete"}},
	actLogs:    {{resource: string(KindPod), subresource: "log", verb: "get"}},
	actPod: {
		{group: groupBatch, resource: "jobs", verb: verbCreate},
		{resource: string(KindPod), subresource: "exec", verb: verbCreate},
		{resource: string(KindPod), subresource: "log", verb: "get"},
	},
	actNewClaim: {{resource: "persistentvolumeclaims", verb: verbCreate}},
}

var watchGroups = map[Kind]string{
	KindScheduledBackup: groupB2Backup,
	KindRepository:      groupB2Backup,
	KindCronJob:         groupBatch,
	KindJob:             groupBatch,
	KindPod:             "",
}

// access is what RBAC lets the user do in the TUI's scope.
type access struct {
	denied map[action]string
	// unwatched are the kinds the user may not list and watch, with why.
	unwatched map[Kind]string
}

func allowAll() access {
	return access{denied: map[action]string{}, unwatched: map[Kind]string{}}
}

func (a access) can(act action) bool { return a.denied[act] == "" }

func (a access) watched() []Kind {
	var kinds []Kind
	for _, k := range []Kind{KindScheduledBackup, KindRepository, KindCronJob, KindJob, KindPod} {
		if a.unwatched[k] == "" {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

// reviewAccess asks the API server, through SelfSubjectAccessReviews, what the
// user may do in namespace, or in every namespace when it is empty. A review
// that fails counts as allowed: the action then fails with the API's own error.
func reviewAccess(ctx context.Context, cs kubernetes.Interface, namespace string) access {
	a := allowAll()
	scope := "in namespace " + namespace
	if namespace == "" {
		scope = "in every namespace"
	}
	allowed := func(g grant) bool {
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: namespace, Group: g.group, Resource: g.resource,
					Subresource: g.subresource, Verb: g.verb,
				},
			},
		}
		res, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		return err != nil || res.Status.Allowed
	}
	for act, grants := range required {
		var missing []string
		for _, g := range grants {
			if !allowed(g) {
				missing = append(missing, g.describe())
			}
		}
		if len(missing) > 0 {
			a.denied[act] = fmt.Sprintf("RBAC does not let you %s %s", strings.Join(missing, " or "), scope)
		}
	}
	for kind, group := range watchGroups {
		for _, verb := range []string{"list", "watch"} {
			if !allowed(grant{group: group, resource: string(kind), verb: verb}) {
				a.unwatched[kind] = fmt.Sprintf("RBAC does not let you %s %s %s", verb, kind, scope)
				break
			}
		}
	}
	return a
}

func (g grant) describe() string {
	what := g.resource
	if g.subresource != "" {
		what += "/" + g.subresource
	}
	return g.verb + " " + what
}
