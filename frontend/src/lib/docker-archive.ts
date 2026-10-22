import { t as translate } from "./i18n";
import type { Capability, Task } from "./api";
import { phase2Capability } from "./database-binlog";

export const DOCKER_ARCHIVE_CHUNK = 32 * 1024;
export const DOCKER_ARCHIVE_MAX_BYTES = 4 * 1024 * 1024 * 1024;

export type DockerArchiveManifest = {
  archive_id: string;
  image: string;
  image_id: string;
  bytes: number;
  sha256: string;
  created_at: string;
  state: string;
  proof: string;
};

export type DockerArchiveRecord = Omit<DockerArchiveManifest, "proof">;

export type DockerArchiveReadChunk = {
  archive_id: string;
  bytes: number;
  sha256: string;
  chunk_sha256: string;
  offset: number;
  next_offset: number;
  eof: boolean;
  data: Uint8Array;
};

export type DockerArchiveUploadStatus = {
  archive: DockerArchiveRecord;
  next_offset: number;
  complete: boolean;
};

export type DockerArchiveChunkReceipt = {
  archive_id: string;
  next_offset: number;
  complete: boolean;
};

type Row = Record<string, unknown>;
const row = (value: unknown): value is Row =>
  !!value && typeof value === "object" && !Array.isArray(value);
const integer = (value: unknown): value is number =>
  typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const digest = (value: unknown): value is string =>
  typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
const archiveID = (value: unknown): value is string =>
  typeof value === "string" && /^dockerarchive_[a-f0-9]{32}$/.test(value);
const imageID = (value: unknown): value is string =>
  typeof value === "string" && /^sha256:[a-f0-9]{64}$/.test(value);
const imageRef = (value: unknown): value is string =>
  typeof value === "string" &&
  /^[a-z0-9][a-z0-9._:/-]*@sha256:[a-f0-9]{64}$/.test(value);
const timestamp = (value: unknown): value is string =>
  typeof value === "string" && Number.isFinite(Date.parse(value));

export function dockerArchiveCapability(
  capabilities: Capability[],
  name: string,
  scope: "containers:read" | "containers:manage",
  fields: string[],
  task: boolean,
  readOnly: boolean,
) {
  const capability = phase2Capability(
    capabilities,
    name,
    scope,
    fields,
    task,
    readOnly,
  );
  const properties = capability?.input_schema?.properties;
  const image = row(properties) ? properties.image : undefined;
  if (
    !capability ||
    !row(image) ||
    typeof image.pattern !== "string" ||
    !image.pattern.includes("@sha256:") ||
    !image.pattern.includes("64")
  )
    return undefined;
  return capability;
}

function parseArchiveRecord(value: unknown): DockerArchiveRecord | undefined {
  if (
    !row(value) ||
    !archiveID(value.archive_id) ||
    !imageRef(value.image) ||
    !imageID(value.image_id) ||
    !integer(value.bytes) ||
    value.bytes < 1 ||
    value.bytes > DOCKER_ARCHIVE_MAX_BYTES ||
    !digest(value.sha256) ||
    !timestamp(value.created_at) ||
    typeof value.state !== "string"
  )
    return undefined;
  return {
    archive_id: value.archive_id,
    image: value.image,
    image_id: value.image_id,
    bytes: value.bytes,
    sha256: value.sha256,
    created_at: value.created_at,
    state: value.state,
  };
}

export function dockerArchiveManifest(
  value: unknown,
): DockerArchiveManifest | undefined {
  const record = parseArchiveRecord(value);
  if (
    !record ||
    !row(value) ||
    typeof value.proof !== "string" ||
    value.proof.length < 32 ||
    value.proof.length > 4096
  )
    return undefined;
  return { ...record, proof: value.proof };
}

export function dockerArchiveManifestText(text: string): DockerArchiveManifest {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    throw new Error(translate("归档清单不是有效的 JSON。"));
  }
  const parsed = dockerArchiveManifest(value);
  if (!parsed) throw new Error(translate("归档清单字段或服务端证明无效。"));
  return parsed;
}

export function dockerArchiveTaskOutput(
  value: unknown,
): Record<string, unknown> | undefined {
  const source = row(value) && row(value.result) ? value.result : value;
  if (!row(source) || typeof source.stdout !== "string") return undefined;
  try {
    const output: unknown = JSON.parse(source.stdout);
    return row(output) ? output : undefined;
  } catch {
    return undefined;
  }
}

export function dockerArchiveTaskID(value: unknown): string | undefined {
  if (!row(value)) return undefined;
  const id = typeof value.id === "string" ? value.id : value.task_id;
  return typeof id === "string" && id ? id : undefined;
}

export function dockerArchiveExportResult(
  task: Task,
): DockerArchiveManifest | undefined {
  return dockerArchiveManifest(dockerArchiveTaskOutput(task.result));
}

export function dockerArchiveImportResult(task: Task):
  | {
      archive_id: string;
      image: string;
      image_id: string;
      loaded: true;
      already_loaded: boolean;
      sha256: string;
      provenance?: "managed_archive" | "engine_digest";
    }
  | undefined {
  const value = dockerArchiveTaskOutput(task.result);
  if (
    !value ||
    !archiveID(value.archive_id) ||
    !imageRef(value.image) ||
    !imageID(value.image_id) ||
    value.loaded !== true ||
    typeof value.already_loaded !== "boolean" ||
    !digest(value.sha256)
  )
    return undefined;
  return {
    archive_id: value.archive_id,
    image: value.image,
    image_id: value.image_id,
    loaded: true,
    already_loaded: value.already_loaded,
    sha256: value.sha256,
    ...(value.provenance === "managed_archive" ||
    value.provenance === "engine_digest"
      ? { provenance: value.provenance }
      : {}),
  };
}

export function dockerArchiveUploadStatus(
  value: unknown,
  archive: { id: string; image: string; sha256: string; bytes: number },
): DockerArchiveUploadStatus | undefined {
  if (!row(value) || !row(value.archive)) return undefined;
  const record = parseArchiveRecord(value.archive);
  if (
    !record ||
    record.archive_id !== archive.id ||
    record.image !== archive.image ||
    record.sha256 !== archive.sha256 ||
    record.bytes !== archive.bytes ||
    !integer(value.next_offset) ||
    value.next_offset > record.bytes ||
    typeof value.complete !== "boolean" ||
    value.complete !== (value.next_offset === record.bytes)
  )
    return undefined;
  return {
    archive: record,
    next_offset: value.next_offset,
    complete: value.complete,
  };
}

export function dockerArchiveUploadBegin(
  value: unknown,
  manifest: DockerArchiveManifest,
): DockerArchiveUploadStatus | undefined {
  if (
    !row(value) ||
    value.chunk_size !== DOCKER_ARCHIVE_CHUNK ||
    value.next_offset !== 0 ||
    value.complete !== false ||
    !row(value.archive) ||
    !archiveID(value.archive.archive_id)
  )
    return undefined;
  const parsed = dockerArchiveUploadStatus(
    {
      archive: value.archive,
      next_offset: value.next_offset,
      complete: value.complete,
    },
    {
      id: value.archive.archive_id,
      image: manifest.image,
      sha256: manifest.sha256,
      bytes: manifest.bytes,
    },
  );
  return parsed?.archive.image_id === manifest.image_id ? parsed : undefined;
}

export function dockerArchiveChunkReceipt(
  value: unknown,
  expected: { id: string; next_offset: number; complete: boolean },
): DockerArchiveChunkReceipt | undefined {
  if (
    !row(value) ||
    value.archive_id !== expected.id ||
    value.next_offset !== expected.next_offset ||
    value.complete !== expected.complete
  )
    return undefined;
  return {
    archive_id: expected.id,
    next_offset: expected.next_offset,
    complete: expected.complete,
  };
}

function decodeBase64(value: unknown): Uint8Array | undefined {
  if (
    typeof value !== "string" ||
    value.length > Math.ceil((DOCKER_ARCHIVE_CHUNK * 4) / 3) + 4 ||
    !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(
      value,
    )
  )
    return undefined;
  try {
    const binary = atob(value);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    return bytes;
  } catch {
    return undefined;
  }
}

export function dockerArchiveReadChunk(
  value: unknown,
  expected: { id: string; offset: number; bytes: number; sha256: string },
): DockerArchiveReadChunk | undefined {
  if (!row(value)) return undefined;
  const data = decodeBase64(value.data);
  if (
    value.archive_id !== expected.id ||
    value.encoding !== "base64" ||
    value.bytes !== expected.bytes ||
    value.sha256 !== expected.sha256 ||
    value.offset !== expected.offset ||
    !integer(value.next_offset) ||
    !data ||
    data.length < 1 ||
    data.length > DOCKER_ARCHIVE_CHUNK ||
    value.next_offset !== expected.offset + data.length ||
    typeof value.eof !== "boolean" ||
    value.eof !== (value.next_offset === expected.bytes) ||
    !digest(value.chunk_sha256)
  )
    return undefined;
  return {
    archive_id: expected.id,
    bytes: expected.bytes,
    sha256: expected.sha256,
    chunk_sha256: value.chunk_sha256,
    offset: expected.offset,
    next_offset: value.next_offset,
    eof: value.eof,
    data,
  };
}

export function dockerArchiveExportResultForImage(
  task: Task,
  image: string,
): DockerArchiveManifest | undefined {
  const manifest = dockerArchiveExportResult(task);
  return manifest?.image === image ? manifest : undefined;
}
