export const API_GROUP = "example.miloapis.com";
export const API_VERSION = "v1alpha1";
export const RESOURCES_BASE = `/apis/${API_GROUP}/${API_VERSION}`;

export function resourcesPath(namespace = "default") {
  return `${RESOURCES_BASE}/namespaces/${namespace}/resources`;
}

export function resourcePath(name: string, namespace = "default") {
  return `${RESOURCES_BASE}/namespaces/${namespace}/resources/${name}`;
}
