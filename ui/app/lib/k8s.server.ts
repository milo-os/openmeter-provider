import https from "https";
import http from "http";
import { getKubeConfig } from "~/lib/kubeconfig.server";

export interface FetchK8sOptions {
  method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  headers?: Record<string, string>;
  body?: string;
}

export async function fetchK8s<T>(
  _request: Request,
  path: string,
  options: FetchK8sOptions = {}
): Promise<T> {
  const config = getKubeConfig();
  const targetUrl = new URL(path, config.apiServerUrl);
  const isHttps = targetUrl.protocol === "https:";
  const method = options.method ?? "GET";

  const headers: Record<string, string> = {
    Accept: "application/json",
    ...(options.headers ?? {}),
  };
  if (config.bearerToken) {
    headers["Authorization"] = `Bearer ${config.bearerToken}`;
  }
  if (options.body && !headers["Content-Type"] && !headers["content-type"]) {
    headers["Content-Type"] = "application/json";
  }
  if (options.body) {
    headers["Content-Length"] = String(Buffer.byteLength(options.body, "utf8"));
  }

  return new Promise<T>((resolve, reject) => {
    const requestOptions: https.RequestOptions = {
      hostname: targetUrl.hostname,
      port: targetUrl.port || (isHttps ? 443 : 80),
      path: targetUrl.pathname + targetUrl.search,
      method,
      headers,
      ...(isHttps
        ? {
            ...(config.caCert ? { ca: config.caCert } : {}),
            ...(config.clientCert && config.clientKey
              ? { cert: config.clientCert, key: config.clientKey }
              : {}),
            rejectUnauthorized: !!config.caCert,
          }
        : {}),
    };

    const transport = isHttps ? https : http;
    const req = transport.request(requestOptions, (res) => {
      const chunks: Buffer[] = [];
      res.on("data", (c) => chunks.push(c));
      res.on("end", () => {
        const body = Buffer.concat(chunks).toString("utf8");
        if (!res.statusCode || res.statusCode >= 400) {
          reject(new Error(`Kubernetes API ${res.statusCode} ${res.statusMessage}: ${body || path}`));
          return;
        }
        if (!body) {
          resolve({} as T);
          return;
        }
        try {
          resolve(JSON.parse(body) as T);
        } catch {
          reject(new Error(`Failed to parse Kubernetes API response for ${path}`));
        }
      });
    });

    req.on("error", reject);
    if (options.body) req.write(options.body);
    req.end();
  });
}
