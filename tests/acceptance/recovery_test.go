package acceptance_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/registry/remote"
)

// Faults reject one publication coordinate persistently until the test removes
// them. This exercises the real registry's partially accepted graph, not an
// in-memory registry implementation or a client retry that hides the failure.
type publicationProbe struct {
	mu        sync.Mutex
	path      string
	denied    int
	mutations int
}

func (p *publicationProbe) reject(w http.ResponseWriter, r *http.Request) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		p.mutations++
	}
	if p.path != "" && r.Method == http.MethodPut && r.URL.Path == p.path {
		p.denied++
		http.Error(w, "injected acceptance publication failure", http.StatusForbidden)
		return true
	}
	return false
}

func (p *publicationProbe) configure(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path, p.denied, p.mutations = path, 0, 0
}

func (p *publicationProbe) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.denied, p.mutations
}

func manifestPath(host, repository, reference string) string {
	return "/v2/" + strings.TrimPrefix(repository, host+"/") + "/manifests/" + reference
}

func runPublicationChecks(t *testing.T, ctx context.Context, client *bale.Client, command cli, probe *publicationProbe, layout, delivery, host string, subject, runtime bale.Root) {
	t.Helper()
	for _, stage := range []string{"graph", "record", "alias", "completion"} {
		t.Run("retry-"+stage, func(t *testing.T) {
			command.t = t
			completion := host + "/retry-" + stage + ":bundle"
			args := []string{"push", layout, completion, "--delivery", delivery, "--format", "json"}
			plan := decode[bale.DeliveryPlan](t, command.run("recovery preview", append(append([]string{}, args...), "--dry-run")...))
			placement, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: runtime.Repository, Digest: runtime.Descriptor.Digest})
			if !ok || len(plan.Tags) < 2 || len(plan.Destinations) == 0 {
				t.Fatal("recovery fixture lacks runtime, aliases or attachments")
			}
			var path string
			switch stage {
			case "graph":
				path = manifestPath(host, placement.Destination.Repository, runtime.Descriptor.Digest.String())
			case "record":
				a := plan.Destinations[0]
				path = manifestPath(host, a.Repository, a.Descriptor.Digest.String())
			case "alias":
				last := plan.Tags[len(plan.Tags)-1]
				path = manifestPath(host, last.Repository, last.Reference)
			case "completion":
				path = "/v2/retry-completion/manifests/bundle"
			}
			probe.configure(path)
			t.Cleanup(func() { probe.configure("") })
			raw, stderr, code := command.execute("expected "+stage+" failure", args...)
			denied, _ := probe.counts()
			if code != 125 || denied == 0 || !strings.Contains(string(stderr), "Error:") {
				t.Fatalf("failure was not surfaced: exit=%d denied=%d stderr=%s", code, denied, stderr)
			}
			partial := decode[bale.PushResult](t, raw)
			if partial.Descriptor.Digest != plan.Descriptor.Digest {
				t.Fatal("partial report lost attempted bundle identity")
			}
			verifyConfirmedPublications(t, ctx, client, plan, partial)
			if _, err := client.Resolve(ctx, completion); !errors.Is(err, bale.ErrNotFound) {
				t.Fatalf("completion visible before successful push: %v", err)
			}
			if stage == "graph" && len(partial.Destinations) != 0 {
				t.Fatal("destination records published before graph verification")
			}
			if stage == "alias" && (len(partial.Tags) != len(plan.Tags)-1 || len(partial.Destinations) != len(plan.Destinations)) {
				t.Fatal("alias failure omitted confirmed earlier tags or attachments")
			}
			if stage == "completion" && len(partial.Tags) != len(plan.Tags) {
				t.Fatal("completion failure omitted confirmed aliases")
			}
			probe.configure("")
			result := decode[bale.PushResult](t, command.run("retry identical push", args...))
			if result.Descriptor.Digest != plan.Descriptor.Digest || len(result.Relocations) != len(plan.Relocations) || len(result.Tags) != len(plan.Tags) || len(result.Destinations) != len(plan.Destinations) {
				t.Fatal("retry did not confirm the complete original plan")
			}
			for _, planned := range plan.Relocations {
				confirmed, ok := result.Relocations.Lookup(planned.Source)
				if !ok || !reflect.DeepEqual(confirmed, planned) {
					t.Fatal("retry omitted or changed a planned relocation")
				}
			}
			for _, planned := range plan.Destinations {
				found := false
				for _, confirmed := range result.Destinations {
					if confirmed.Repository == planned.Repository && confirmed.Subject.Digest == planned.Subject.Digest && confirmed.Descriptor.Digest == planned.Descriptor.Digest && confirmed.Published && confirmed.Discoverable {
						found = true
					}
				}
				if !found {
					t.Fatal("retry omitted a discoverable planned attachment")
				}
			}
			if !reflect.DeepEqual(result.Tags, plan.Tags) {
				t.Fatal("retry omitted or changed a planned alias")
			}
			if (stage == "alias" || stage == "completion") && result.Stats.BlobBytesUploaded != 0 {
				t.Fatalf("retry reuploaded completed blob content: %+v", result.Stats)
			}
			verifyConfirmedPublications(t, ctx, client, plan, result)
			for _, root := range []bale.Root{subject, runtime} {
				where, ok := plan.Relocations.Lookup(bale.ArtifactID{Repository: root.Repository, Digest: root.Descriptor.Digest})
				if !ok {
					t.Fatal("retry omitted canonical root")
				}
				resolved, err := client.Resolve(ctx, where.Destination.Repository+"@"+root.Descriptor.Digest.String())
				if err != nil {
					t.Fatal(err)
				}
				verifyGraph(t, ctx, root.Store, resolved.Store, root.Descriptor)
			}
			pulled := decode[bale.PullResult](t, command.run("pull recovered completion", "pull", completion, t.TempDir()+"/recovered", "--format", "json"))
			if pulled.Descriptor.Digest != plan.Descriptor.Digest {
				t.Fatal("recovered bundle changed digest")
			}
		})
	}
	for _, collision := range []string{"alias", "completion", "inventory"} {
		t.Run("conflict-"+collision, func(t *testing.T) {
			command.t = t
			name := "conflict-" + collision
			completion := host + "/" + name + ":bundle"
			args := []string{"push", layout, completion, "--delivery", delivery, "--format", "json"}
			plan := decode[bale.DeliveryPlan](t, command.run("conflict preview", append(append([]string{}, args...), "--dry-run")...))
			occupied := bale.Tag{Repository: host + "/" + name, Reference: "bundle"}
			switch collision {
			case "alias":
				occupied = plan.Tags[0]
			case "inventory":
				a := plan.Destinations[0]
				occupied = bale.Tag{Repository: a.Repository, Reference: strings.Replace(a.Subject.Digest.String(), ":", "-", 1)}
			}
			// Publish an unrelated real graph, then let an external producer own
			// the occupied tag. Bale must preserve it without any destination write.
			fixture := subject
			if occupied.Descriptor.Digest == fixture.Descriptor.Digest {
				fixture = runtime
			}
			copyPlan, err := client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{fixture}, Destination: host, RepositoryMap: map[string]string{fixture.Repository: strings.TrimPrefix(occupied.Repository, host+"/")}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Execute(ctx, copyPlan); err != nil {
				t.Fatal(err)
			}
			repository, err := remote.NewRepository(occupied.Repository)
			if err != nil {
				t.Fatal(err)
			}
			repository.PlainHTTP, repository.Client = true, &http.Client{Timeout: 10 * time.Second}
			if err := repository.Tag(ctx, fixture.Descriptor, occupied.Reference); err != nil {
				t.Fatal(err)
			}
			probe.configure("")
			_, stderr, code := command.execute("reject existing "+collision, args...)
			_, mutations := probe.counts()
			if code != 125 || mutations != 0 || !strings.Contains(string(stderr), "Error:") {
				t.Fatalf("conflict modified destination: exit=%d mutations=%d stderr=%s", code, mutations, stderr)
			}
			resolved, err := client.Resolve(ctx, occupied.Repository+":"+occupied.Reference)
			if err != nil || resolved.Descriptor.Digest != fixture.Descriptor.Digest {
				t.Fatalf("conflicting tag was changed: %v", err)
			}
		})
	}
}

func verifyConfirmedPublications(t *testing.T, ctx context.Context, client *bale.Client, plan bale.DeliveryPlan, result bale.PushResult) {
	t.Helper()
	for _, confirmed := range result.Relocations {
		planned, ok := plan.Relocations.Lookup(confirmed.Source)
		if !ok || !reflect.DeepEqual(confirmed, planned) {
			t.Fatal("report claimed an unplanned relocation")
		}
		if _, err := client.Resolve(ctx, confirmed.Destination.Repository+"@"+confirmed.Destination.Digest.String()); err != nil {
			t.Fatalf("report claimed an unavailable relocation: %v", err)
		}
	}
	for _, tag := range result.Tags {
		resolved, err := client.Resolve(ctx, tag.Repository+":"+tag.Reference)
		if err != nil || resolved.Descriptor.Digest != tag.Descriptor.Digest {
			t.Fatalf("report claimed an unavailable alias: %+v %v", tag, err)
		}
	}
	for _, attachment := range result.Destinations {
		if attachment.Published {
			if _, err := client.Resolve(ctx, attachment.Repository+"@"+attachment.Descriptor.Digest.String()); err != nil {
				t.Fatalf("report claimed an unavailable attachment: %v", err)
			}
		}
		if attachment.Discoverable {
			refs, err := client.ListReferrers(ctx, attachment.Repository+"@"+attachment.Subject.Digest.String(), bale.ListReferrersOptions{})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, ref := range refs {
				if ref.Digest == attachment.Descriptor.Digest {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("attachment was not discoverable exactly once: %s (%d)", attachment.Descriptor.Digest, count)
			}
		}
	}
}
