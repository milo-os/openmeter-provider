import { json } from "@remix-run/node";
import type { LoaderFunctionArgs } from "@remix-run/node";
import { useLoaderData } from "@remix-run/react";
import { Badge } from "@datum-cloud/datum-ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@datum-cloud/datum-ui/card";
import { PageTitle } from "@datum-cloud/datum-ui/page-title";
import { fetchK8s } from "~/lib/k8s.server";
import { phaseBadgeProps, relativeAge } from "~/lib/format";
import type { Resource } from "~/lib/types";
import { resourcePath } from "~/lib/api";

interface LoaderData {
  resource: Resource | null;
  error?: string;
}

export async function loader({ request, params }: LoaderFunctionArgs) {
  const name = params.name ?? "";
  try {
    // Resources are namespaced — list all and find by name for the template.
    // In practice, scope this to a specific namespace via query param or config.
    const resource = await fetchK8s<Resource>(
      request,
      resourcePath(encodeURIComponent(name))
    );
    return json({ resource } satisfies LoaderData);
  } catch (e) {
    return json({
      resource: null,
      error: e instanceof Error ? e.message : String(e),
    } satisfies LoaderData);
  }
}

export default function ResourceDetail() {
  const { resource, error } = useLoaderData<typeof loader>() as LoaderData;

  if (error || !resource) {
    return (
      <div className="px-6 py-4">
        <Card>
          <CardContent className="py-6">
            <p className="text-sm font-medium">Failed to load resource</p>
            <p className="text-sm text-muted-foreground mt-1">{error}</p>
          </CardContent>
        </Card>
      </div>
    );
  }

  const phase = resource.status?.phase;
  const badge = phase ? phaseBadgeProps(phase) : null;

  return (
    <div className="flex flex-col gap-4 px-6 py-4">
      <div className="flex items-center gap-3">
        <PageTitle title={resource.metadata.name} actionsPosition="inline" />
        {badge && phase && (
          <Badge type={badge.type} theme={badge.theme}>
            {phase}
          </Badge>
        )}
      </div>

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Details</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="space-y-2 text-sm">
              <div className="flex justify-between">
                <dt className="text-muted-foreground">Namespace</dt>
                <dd>{resource.metadata.namespace ?? "—"}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-muted-foreground">Age</dt>
                <dd>{relativeAge(resource.metadata.creationTimestamp)}</dd>
              </div>
              {resource.spec.description && (
                <div className="flex justify-between">
                  <dt className="text-muted-foreground">Description</dt>
                  <dd className="text-right max-w-[60%]">{resource.spec.description}</dd>
                </div>
              )}
            </dl>
          </CardContent>
        </Card>

        {resource.status?.conditions && resource.status.conditions.length > 0 && (
          <Card>
            <CardHeader>
              <CardTitle>Conditions</CardTitle>
            </CardHeader>
            <CardContent>
              <dl className="space-y-2 text-sm">
                {resource.status.conditions.map((c) => (
                  <div key={c.type} className="flex justify-between">
                    <dt className="text-muted-foreground">{c.type}</dt>
                    <dd className="flex items-center gap-2">
                      <span
                        className={
                          c.status === "True"
                            ? "text-green-600 dark:text-green-400"
                            : "text-muted-foreground"
                        }
                      >
                        {c.status}
                      </span>
                      {c.reason && (
                        <span className="text-muted-foreground">({c.reason})</span>
                      )}
                    </dd>
                  </div>
                ))}
              </dl>
            </CardContent>
          </Card>
        )}
      </div>
    </div>
  );
}
