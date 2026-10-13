export type Page<T> = {
  items: T[];
  total?: number;
  limit?: number;
  offset?: number;
};
export type Site = {
  id: string;
  name: string;
  domains: string[];
  status: string;
  web_instance?: string;
  default?: boolean;
  document_root: string;
  php_version?: string;
  php_instance?: string;
  revision: number;
  configuration_mode?: "managed" | "advanced";
  tls?: {
    enabled: boolean;
    expires_at?: string;
    issuer?: string;
    certificate_id?: string;
    certificate_revision?: number;
  };
  updated_at: string;
  config?: unknown;
  [key: string]: unknown;
};
export type CatalogVersion = {
  id: string;
  version: string;
  default?: boolean;
  source?: string | Record<string, unknown>;
  verification?: { status: string; reason?: string };
};
export type CatalogItem = {
  id: string;
  name: string;
  description: string;
  category: string;
  integration_version: string;
  versions: CatalogVersion[];
  platforms: (
    | string
    | { os: string; arch: string; distro?: string; release?: string }
  )[];
  capabilities: (string | { name: string; status?: string; reason?: string })[];
  verification?: { status: string; reason?: string };
  [key: string]: unknown;
};
export type SoftwareInstance = {
  id: string;
  plugin_id: string;
  instance_id: string;
  status: string;
  installed_version: string;
  health?: string | { status: string; reason?: string };
  updated_at: string;
  config?: unknown;
  actions?: unknown;
  [key: string]: unknown;
};
export type FileRoot = {
  id: string;
  label: string;
  path?: string;
  read_only?: boolean;
  [key: string]: unknown;
};
export type FileEntry = {
  name: string;
  path: string;
  type: "file" | "directory" | "symlink" | string;
  size: number;
  mode?: number | string;
  owner?: number | string;
  group?: number | string;
  modified_at: string;
  sha256?: string;
  [key: string]: unknown;
};
export type FilePage = Page<FileEntry> & { root: string; path: string };
export type FileContent = {
  content: string;
  encoding: string;
  sha256: string;
  size?: number;
  truncated?: boolean;
  [key: string]: unknown;
};
export type TerminalTarget = {
  id: string;
  name?: string;
  label?: string;
  type?: string;
  kind?: string;
  host?: string;
  username?: string;
  user?: string;
  port?: number;
  [key: string]: unknown;
};
export type TerminalSession = {
  id: string;
  target_id: string;
  actor_id: string;
  expires_at: string;
  sequence: number;
};
