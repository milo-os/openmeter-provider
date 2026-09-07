import { json } from "@remix-run/node";
import type { LoaderFunctionArgs } from "@remix-run/node";
import { Link, useLoaderData } from "@remix-run/react";
import { useMemo, useState } from "react";
import { Badge } from "@datum-cloud/datum-ui/badge";
import { Card, CardContent } from "@datum-cloud/datum-ui/card";
import { EmptyContent } from "@datum-cloud/datum-ui/empty-content";
import { Input } from "@datum-cloud/datum-ui/input";
import { PageTitle } from "@datum-cloud/datum-ui/page-title";
import { Search } from "lucide-react";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@datum-cloud/datum-ui/table";
import { fetchK8s } from "~/lib/k8s.server";
import { phaseBadgeProps, relativeAge } from "~/lib/format";
import type { KubeList, Resource } from "~/lib/types";
import { resourcesPath } from "~/lib/api";

interface LoaderData {
  resources: Resource[];
  error?: string;
}

export async function loader({ request }: LoaderFunctionArgs) {
  try {
    const result = await fetchK8s<KubeList<Resource>>(
      request,
      resourcesPath()
    );
    return json({ resources: result.items ?? [] } satisfies LoaderData);
  } catch (e) {
    return json({
      resources: [],
      error: e instanceof Error ? e.message : String(e),
    } satisfies LoaderData);
  }
}

function matchesQuery(resource: Resource, q: string): boolean {
  if (!q) return true;
  const needle = q.toLowerCase();
  return [resource.metadata.name, resource.spec.description ?? ""].some((h) =>
    h.toLowerCase().includes(needle)
  );
}

export default function ResourcesIndex() {
  const { resources, error } = useLoaderData<typeof loader>() as LoaderData;
  const [query, setQuery] = useState("");

  const filtered = useMemo(
    () => resources.filter((r) => matchesQuery(r, query.trim())),
    [resources, query]
  );

  return (
    <div className="flex flex-col gap-4 px-6 py-4">
      <PageTitle
        title="Resources"
        description="Resources managed by the controller on the Milo control plane."
        actionsPosition="inline"
      />

      {error ? (
        <Card>
          <CardContent className="py-6">
            <p className="text-sm font-medium">Failed to load resources</p>
            <p className="text-sm text-muted-foreground mt-1">{error}</p>
            <a href="/resources" className="text-sm text-primary underline mt-2 inline-block">
              Retry
            </a>
          </CardContent>
        </Card>
      ) : resources.length === 0 ? (
        <EmptyContent
          title="no resources found."
          subtitle="Resources will appear here once they are created on the Milo control plane."
          size="lg"
        />
      ) : (
        <>
          <div className="relative max-w-sm">
            <Search className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
            <Input
              type="search"
              placeholder="Search resources…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              className="pl-9"
            />
          </div>
          {filtered.length === 0 ? (
            <Card>
              <CardContent className="flex flex-col items-center gap-3 py-8">
                <p className="text-sm font-medium">
                  No matches for &ldquo;{query}&rdquo;.
                </p>
              </CardContent>
            </Card>
          ) : (
            <Card>
              <CardContent className="p-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Name</TableHead>
                      <TableHead>Namespace</TableHead>
                      <TableHead>Phase</TableHead>
                      <TableHead>Age</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {filtered.map((r) => {
                      const phase = phaseBadgeProps(r.status?.phase ?? "");
                      return (
                        <TableRow key={`${r.metadata.namespace}/${r.metadata.name}`}>
                          <TableCell>
                            <Link
                              to={`/resources/${encodeURIComponent(r.metadata.name)}`}
                              className="text-primary hover:underline"
                            >
                              {r.metadata.name}
                            </Link>
                          </TableCell>
                          <TableCell className="text-muted-foreground">
                            {r.metadata.namespace ?? "—"}
                          </TableCell>
                          <TableCell>
                            {r.status?.phase ? (
                              <Badge type={phase.type} theme={phase.theme}>
                                {r.status.phase}
                              </Badge>
                            ) : (
                              "—"
                            )}
                          </TableCell>
                          <TableCell>
                            {relativeAge(r.metadata.creationTimestamp)}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
