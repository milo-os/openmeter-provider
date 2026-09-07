export interface KubeMeta {
  name: string;
  namespace?: string;
  creationTimestamp: string;
}

export interface KubeList<T> {
  items: T[];
}

export interface KubeCondition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
}

export interface Resource {
  metadata: KubeMeta;
  spec: {
    description?: string;
  };
  status?: {
    phase?: string;
    conditions?: KubeCondition[];
    observedGeneration?: number;
  };
}
