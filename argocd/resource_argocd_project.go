package argocd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	projectClient "github.com/argoproj/argo-cd/v2/pkg/apiclient/project"
	application "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func resourceArgoCDProject() *schema.Resource {
	return &schema.Resource{
		Description:   "Manages [projects](https://argo-cd.readthedocs.io/en/stable/user-guide/projects/) within ArgoCD.",
		CreateContext: resourceArgoCDProjectCreate,
		ReadContext:   resourceArgoCDProjectRead,
		UpdateContext: resourceArgoCDProjectUpdate,
		DeleteContext: resourceArgoCDProjectDelete,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Schema: map[string]*schema.Schema{
			"metadata": metadataSchema("appprojects.argoproj.io"),
			"spec":     projectSpecSchemaV2(),
		},
		SchemaVersion: 2,
		StateUpgraders: []schema.StateUpgrader{
			{
				Type:    resourceArgoCDProjectV0().CoreConfigSchema().ImpliedType(),
				Upgrade: resourceArgoCDProjectStateUpgradeV0,
				Version: 0,
			},
			{
				Type:    resourceArgoCDProjectV1().CoreConfigSchema().ImpliedType(),
				Upgrade: resourceArgoCDProjectStateUpgradeV1,
				Version: 1,
			},
		},
	}
}

func resourceArgoCDProjectCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	si := meta.(*ServerInterface)
	if err := si.initClients(ctx); err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  "failed to init clients",
				Detail:   err.Error(),
			},
		}
	}

	objectMeta, spec, err := expandProject(d)
	if err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("project %s could not be created", d.Id()),
				Detail:   err.Error(),
			},
		}
	}

	projectName := objectMeta.Name

	if _, ok := tokenMutexProjectMap[projectName]; !ok {
		tokenMutexProjectMap[projectName] = &sync.RWMutex{}
	}

	tokenMutexProjectMap[projectName].RLock()
	p, err := si.ProjectClient.Get(ctx, &projectClient.ProjectQuery{
		Name: projectName,
	})
	tokenMutexProjectMap[projectName].RUnlock()

	if err != nil {
		if !strings.Contains(err.Error(), "NotFound") {
			return []diag.Diagnostic{
				{
					Severity: diag.Error,
					Summary:  fmt.Sprintf("Project %s could not be created", projectName),
					Detail:   err.Error(),
				},
			}
		}
	}

	if p != nil {
		switch p.DeletionTimestamp {
		case nil:
		default:
			// Pre-existing project is still in Kubernetes soft deletion queue
			time.Sleep(time.Duration(*p.DeletionGracePeriodSeconds))
		}
	}

	featureProjectSourceNamespacesSupported, err := si.isFeatureSupported(featureProjectSourceNamespaces)
	if err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  "feature not supported",
				Detail:   err.Error(),
			},
		}
	} else if !featureProjectSourceNamespacesSupported {
		_, sourceNamespacesOk := d.GetOk("spec.0.source_namespaces")
		if sourceNamespacesOk {
			return []diag.Diagnostic{
				{
					Severity: diag.Error,
					Summary: fmt.Sprintf(
						"project source_namespaces is only supported from ArgoCD %s onwards",
						featureVersionConstraintsMap[featureProjectSourceNamespaces].String()),
				},
			}
		}
	}

	tokenMutexProjectMap[projectName].Lock()
	p, err = si.ProjectClient.Create(ctx, &projectClient.ProjectCreateRequest{
		Project: &application.AppProject{
			ObjectMeta: objectMeta,
			Spec:       spec,
		},
		// TODO: allow upsert instead of always requiring resource import?
		// TODO: make that a resource flag with proper acceptance tests
		Upsert: false,
	})
	tokenMutexProjectMap[projectName].Unlock()

	if err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("project %s could not be created", objectMeta.Name),
				Detail:   err.Error(),
			},
		}
	} else if p == nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("something went wrong during project creation with ID %s", d.Id()),
				Detail:   err.Error(),
			},
		}
	}

	d.SetId(p.Name)

	return resourceArgoCDProjectRead(ctx, d, meta)
}

func resourceArgoCDProjectRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	si := meta.(*ServerInterface)
	if err := si.initClients(ctx); err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  "failed to init clients",
				Detail:   err.Error(),
			},
		}
	}

	projectName := d.Id()

	if _, ok := tokenMutexProjectMap[projectName]; !ok {
		tokenMutexProjectMap[projectName] = &sync.RWMutex{}
	}

	tokenMutexProjectMap[projectName].RLock()
	p, err := si.ProjectClient.Get(ctx, &projectClient.ProjectQuery{
		Name: projectName,
	})
	tokenMutexProjectMap[projectName].RUnlock()

	if err != nil {
		if strings.Contains(err.Error(), "NotFound") {
			d.SetId("")
			return diag.Diagnostics{}
		}

		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("project %s could not be found", projectName),
				Detail:   err.Error(),
			},
		}
	}

	if err = flattenProject(p, d); err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("project %s could not be flattened", d.Id()),
				Detail:   err.Error(),
			},
		}
	}

	return nil
}

func resourceArgoCDProjectUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	if ok := d.HasChanges("metadata", "spec"); !ok {
		return resourceArgoCDProjectRead(ctx, d, meta)
	}

	si := meta.(*ServerInterface)
	if err := si.initClients(ctx); err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  "failed to init clients",
				Detail:   err.Error(),
			},
		}
	}

	objectMeta, spec, err := expandProject(d)
	if err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("project %s could not be updated", d.Id()),
				Detail:   err.Error(),
			},
		}
	}

	projectName := objectMeta.Name

	if _, ok := tokenMutexProjectMap[projectName]; !ok {
		tokenMutexProjectMap[projectName] = &sync.RWMutex{}
	}

	projectRequest := &projectClient.ProjectUpdateRequest{
		Project: &application.AppProject{
			ObjectMeta: objectMeta,
			Spec:       spec,
		},
	}

	tokenMutexProjectMap[projectName].RLock()
	p, err := si.ProjectClient.Get(ctx, &projectClient.ProjectQuery{
		Name: d.Id(),
	})
	tokenMutexProjectMap[projectName].RUnlock()

	if err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  "failed to get project",
				Detail:   err.Error(),
			},
		}
	}

	if p != nil {
		// Kubernetes API requires providing the up-to-date correct ResourceVersion for updates
		projectRequest.Project.ResourceVersion = p.ResourceVersion

		// Preserve preexisting JWTs for managed roles
		roles := expandProjectRoles(d.Get("spec.0.role").([]interface{}))

		for _, r := range roles {
			var pr *application.ProjectRole

			var i int

			pr, i, err = p.GetRoleByName(r.Name)
			if err != nil {
				// i == -1 means the role does not exist
				// and was recently added within Terraform tf files
				if i != -1 {
					return []diag.Diagnostic{
						{
							Severity: diag.Error,
							Summary:  fmt.Sprintf("project role %s could not be retrieved", r.Name),
							Detail:   err.Error(),
						},
					}
				}
			} else { // Only preserve preexisting JWTs for managed roles if we found an existing matching project
				projectRequest.Project.Spec.Roles[i].JWTTokens = pr.JWTTokens
			}
		}
	}

	tokenMutexProjectMap[projectName].Lock()
	_, err = si.ProjectClient.Update(ctx, projectRequest)
	tokenMutexProjectMap[projectName].Unlock()

	if err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("Error while waiting for project %s to be created", projectName),
				Detail:   err.Error(),
			},
		}
	}

	return resourceArgoCDProjectRead(ctx, d, meta)
}

func resourceArgoCDProjectDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	si := meta.(*ServerInterface)
	if err := si.initClients(ctx); err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  "failed to init clients",
				Detail:   err.Error(),
			},
		}
	}

	projectName := d.Id()

	if _, ok := tokenMutexProjectMap[projectName]; !ok {
		tokenMutexProjectMap[projectName] = &sync.RWMutex{}
	}

	tokenMutexProjectMap[projectName].Lock()
	_, err := si.ProjectClient.Delete(ctx, &projectClient.ProjectQuery{Name: projectName})
	tokenMutexProjectMap[projectName].Unlock()

	if err != nil && !strings.Contains(err.Error(), "NotFound") {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("Project %s not found", projectName),
				Detail:   err.Error(),
			},
		}
	}

	d.SetId("")

	return nil
}
