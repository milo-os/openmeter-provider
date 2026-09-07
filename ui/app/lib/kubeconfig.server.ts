import { readFileSync, existsSync } from "fs";
import { load } from "js-yaml";
import { join } from "path";

interface KubeConfig {
  apiServerUrl: string;
  clientCert: Buffer | undefined;
  clientKey: Buffer | undefined;
  caCert: Buffer | undefined;
  bearerToken: string | undefined;
}

let cachedConfig: KubeConfig | null = null;

const IN_CLUSTER_TOKEN_PATH = "/var/run/secrets/kubernetes.io/serviceaccount/token";
const IN_CLUSTER_CA_PATH = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt";

/**
 * Resolves API server connection details from environment, in-cluster
 * ServiceAccount, or a local kubeconfig (in that order).
 *
 * Environment variables:
 * - CONTROLLER_TEMPLATE_API_SERVER_URL
 * - CONTROLLER_TEMPLATE_API_CA_FILE
 * - CONTROLLER_TEMPLATE_API_CERT_FILE
 * - CONTROLLER_TEMPLATE_API_KEY_FILE
 * - CONTROLLER_TEMPLATE_API_TOKEN_FILE
 */
export function getKubeConfig(): KubeConfig {
  if (cachedConfig) {
    return cachedConfig;
  }

  const envApiServerUrl = process.env.CONTROLLER_TEMPLATE_API_SERVER_URL;
  if (envApiServerUrl) {
    console.log("Using API server from environment:", envApiServerUrl);

    let caCert: Buffer | undefined;
    let clientCert: Buffer | undefined;
    let clientKey: Buffer | undefined;
    let bearerToken: string | undefined;

    const caFile = process.env.CONTROLLER_TEMPLATE_API_CA_FILE;
    if (caFile) {
      try { caCert = readFileSync(caFile); } catch { /* optional */ }
    }

    const tokenFile = process.env.CONTROLLER_TEMPLATE_API_TOKEN_FILE;
    if (tokenFile) {
      try { bearerToken = readFileSync(tokenFile, "utf8").trim(); } catch { /* optional */ }
    }

    if (!bearerToken) {
      const certFile = process.env.CONTROLLER_TEMPLATE_API_CERT_FILE;
      const keyFile = process.env.CONTROLLER_TEMPLATE_API_KEY_FILE;
      if (certFile && keyFile) {
        try {
          clientCert = readFileSync(certFile);
          clientKey = readFileSync(keyFile);
        } catch { /* optional */ }
      }
    }

    cachedConfig = { apiServerUrl: envApiServerUrl, clientCert, clientKey, caCert, bearerToken };
    return cachedConfig;
  }

  const k8sHost = process.env.KUBERNETES_SERVICE_HOST;
  const k8sPort = process.env.KUBERNETES_SERVICE_PORT;
  if (k8sHost && k8sPort && existsSync(IN_CLUSTER_TOKEN_PATH)) {
    console.log("Detected in-cluster environment, using ServiceAccount authentication");

    let caCert: Buffer | undefined;
    let bearerToken: string | undefined;
    try { caCert = readFileSync(IN_CLUSTER_CA_PATH); } catch { /* optional */ }
    try { bearerToken = readFileSync(IN_CLUSTER_TOKEN_PATH, "utf8").trim(); } catch { /* optional */ }

    const apiServerUrl = `https://${k8sHost}:${k8sPort}`;
    cachedConfig = { apiServerUrl, clientCert: undefined, clientKey: undefined, caCert, bearerToken };
    return cachedConfig;
  }

  let apiServerUrl = "https://127.0.0.1:6443";
  let clientCert: Buffer | undefined;
  let clientKey: Buffer | undefined;
  let caCert: Buffer | undefined;
  let bearerToken: string | undefined;

  try {
    const kubeconfigPath = join(process.cwd(), "../.test-infra/kubeconfig");
    const kubeconfig = load(readFileSync(kubeconfigPath, "utf8")) as {
      clusters: Array<{ cluster: { server: string; "certificate-authority-data"?: string } }>;
      users: Array<{ user: { "client-certificate-data"?: string; "client-key-data"?: string; token?: string } }>;
    };

    apiServerUrl = kubeconfig.clusters[0].cluster.server;

    const token = kubeconfig.users[0].user.token;
    if (token) {
      bearerToken = token;
    } else {
      const certData = kubeconfig.users[0].user["client-certificate-data"];
      const keyData = kubeconfig.users[0].user["client-key-data"];
      if (certData) clientCert = Buffer.from(certData, "base64");
      if (keyData) clientKey = Buffer.from(keyData, "base64");
    }

    const caData = kubeconfig.clusters[0].cluster["certificate-authority-data"];
    if (caData) caCert = Buffer.from(caData, "base64");

    console.log("Loaded kubeconfig from:", kubeconfigPath);
  } catch {
    console.warn("Could not read kubeconfig, using default:", apiServerUrl);
  }

  cachedConfig = { apiServerUrl, clientCert, clientKey, caCert, bearerToken };
  return cachedConfig;
}

export function clearKubeConfigCache(): void {
  cachedConfig = null;
}
