import type { TFunction } from "i18next"
import { toast } from "sonner"

import type { ChatAttachment } from "@/store/chat"

const CHAT_IMAGE_MIME_TYPES = [
  "image/jpeg",
  "image/png",
  "image/gif",
  "image/webp",
  "image/bmp",
] as const

const CHAT_IMAGE_MIME_TYPE_SET = new Set<string>(CHAT_IMAGE_MIME_TYPES)
const CHAT_IMAGE_EXTENSION_BY_MIME: Record<string, string> = {
  "image/jpeg": ".jpg",
  "image/png": ".png",
  "image/gif": ".gif",
  "image/webp": ".webp",
  "image/bmp": ".bmp",
}
const CHAT_IMAGE_MIME_BY_EXTENSION: Record<string, string> = {
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".png": "image/png",
  ".gif": "image/gif",
  ".webp": "image/webp",
  ".bmp": "image/bmp",
}

export const CHAT_IMAGE_ACCEPT = CHAT_IMAGE_MIME_TYPES.join(",")

const CHAT_TEXT_FILE_MIME_TYPES = [
  "application/json",
  "application/x-ndjson",
  "application/xml",
  "application/yaml",
  "application/x-yaml",
  "application/toml",
  "application/javascript",
  "application/typescript",
  "application/x-sh",
  "application/x-shellscript",
  "application/sql",
  "application/csv",
] as const

const CHAT_TEXT_FILE_MIME_TYPE_SET = new Set<string>(CHAT_TEXT_FILE_MIME_TYPES)

const CHAT_TEXT_FILE_EXTENSIONS = new Set<string>([
  ".txt",
  ".md",
  ".markdown",
  ".mdx",
  ".json",
  ".jsonl",
  ".jsonc",
  ".yaml",
  ".yml",
  ".toml",
  ".ini",
  ".cfg",
  ".conf",
  ".properties",
  ".env",
  ".csv",
  ".tsv",
  ".log",
  ".xml",
  ".html",
  ".htm",
  ".css",
  ".scss",
  ".less",
  ".js",
  ".mjs",
  ".cjs",
  ".ts",
  ".tsx",
  ".jsx",
  ".py",
  ".pyw",
  ".rb",
  ".go",
  ".rs",
  ".java",
  ".kt",
  ".swift",
  ".scala",
  ".c",
  ".h",
  ".cpp",
  ".hpp",
  ".cc",
  ".cs",
  ".php",
  ".lua",
  ".pl",
  ".r",
  ".sh",
  ".bash",
  ".zsh",
  ".sql",
  ".bat",
  ".cmd",
  ".ps1",
  ".proto",
  ".tf",
  ".gradle",
  ".lock",
])

const CHAT_TEXT_FILE_NAMES = new Set<string>([
  "makefile",
  "dockerfile",
  "license",
  "readme",
  "changelog",
  ".gitignore",
  ".gitconfig",
  ".editorconfig",
  ".npmrc",
  ".nvmrc",
  ".bashrc",
  ".profile",
])

export const CHAT_FILE_ACCEPT = [
  ...CHAT_IMAGE_MIME_TYPES,
  "text/*",
  ...CHAT_TEXT_FILE_MIME_TYPES,
  ...CHAT_TEXT_FILE_EXTENSIONS,
  ...CHAT_TEXT_FILE_NAMES,
].join(",")

const MAX_CHAT_IMAGE_SIZE_BYTES = 7 * 1024 * 1024
const MAX_CHAT_IMAGE_SIZE_LABEL = "7 MB"

// Keep the inline text limit aligned with the pico channel backend
// (maxInlineFileChars) so files are never silently clipped server-side.
const MAX_INLINE_FILE_CHARS = 65536
const MAX_INLINE_FILE_TEXT_NOTE =
  "[... truncated by picoclaw: inline file text limit reached ...]"

const MAX_CHAT_TEXT_FILE_SIZE_BYTES = 2 * 1024 * 1024
const MAX_CHAT_TEXT_FILE_SIZE_LABEL = "2 MB"

function readFileAsDataUrl(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => {
      if (typeof reader.result === "string") {
        resolve(reader.result)
        return
      }
      reject(new Error("Failed to read file"))
    }
    reader.onerror = () =>
      reject(reader.error || new Error("Failed to read file"))
    reader.readAsDataURL(file)
  })
}

function readFileAsText(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => {
      if (typeof reader.result === "string") {
        resolve(reader.result)
        return
      }
      reject(new Error("Failed to read file"))
    }
    reader.onerror = () =>
      reject(reader.error || new Error("Failed to read file"))
    reader.readAsText(file, "utf-8")
  })
}

function getFileExtension(fileName: string): string {
  const lastDotIndex = fileName.lastIndexOf(".")
  if (lastDotIndex === -1) {
    return ""
  }
  return fileName.slice(lastDotIndex).toLowerCase()
}

function getSupportedImageMimeType(file: File): string | null {
  const normalizedType = file.type.trim().toLowerCase()
  if (normalizedType && CHAT_IMAGE_MIME_TYPE_SET.has(normalizedType)) {
    return normalizedType
  }

  const extension = getFileExtension(file.name)
  return CHAT_IMAGE_MIME_BY_EXTENSION[extension] ?? null
}

function isSupportedTextFile(file: File): boolean {
  const normalizedType = file.type.trim().toLowerCase()
  if (normalizedType) {
    if (normalizedType.startsWith("text/")) {
      return true
    }
    if (CHAT_TEXT_FILE_MIME_TYPE_SET.has(normalizedType)) {
      return true
    }
  }

  if (CHAT_TEXT_FILE_EXTENSIONS.has(getFileExtension(file.name))) {
    return true
  }

  return CHAT_TEXT_FILE_NAMES.has(file.name.trim().toLowerCase())
}

function normalizeImageFileForDataUrl(file: File, filename: string): File {
  const mimeType = getSupportedImageMimeType(file)
  if (!mimeType || file.type.trim().toLowerCase() === mimeType) {
    return file
  }

  const normalizedName = file.name.trim() || filename
  return new File([file], normalizedName, { type: mimeType })
}

function getAttachmentFilename(file: File, index: number): string {
  const trimmedName = file.name.trim()
  if (trimmedName) {
    return trimmedName
  }

  const mimeType = getSupportedImageMimeType(file)
  const extension = mimeType ? CHAT_IMAGE_EXTENSION_BY_MIME[mimeType] : ".png"
  return `image-${index + 1}${extension}`
}

function getTransferItemFiles(dataTransfer: DataTransfer | null): File[] {
  if (!dataTransfer) {
    return []
  }

  const files = Array.from(dataTransfer.files)
  if (files.length > 0) {
    return files
  }

  return Array.from(dataTransfer.items)
    .filter((item) => item.kind === "file")
    .map((item) => item.getAsFile())
    .filter((file): file is File => file !== null)
}

export function hasFileTransfer(dataTransfer: DataTransfer | null): boolean {
  if (!dataTransfer) {
    return false
  }

  if (dataTransfer.files.length > 0) {
    return true
  }

  return Array.from(dataTransfer.items).some((item) => item.kind === "file")
}

export function getTransferredFiles(dataTransfer: DataTransfer | null) {
  return getTransferItemFiles(dataTransfer)
}

export async function buildChatAttachments(
  files: readonly File[],
  t: TFunction,
): Promise<ChatAttachment[]> {
  const nextAttachments: ChatAttachment[] = []

  for (const [index, file] of files.entries()) {
    const filename = getAttachmentFilename(file, index)

    const imageMimeType = getSupportedImageMimeType(file)
    if (imageMimeType) {
      if (file.size > MAX_CHAT_IMAGE_SIZE_BYTES) {
        toast.error(
          t("chat.imageTooLarge", {
            name: filename,
            size: MAX_CHAT_IMAGE_SIZE_LABEL,
          }),
        )
        continue
      }

      try {
        const normalizedFile = normalizeImageFileForDataUrl(file, filename)
        nextAttachments.push({
          type: "image",
          filename,
          url: await readFileAsDataUrl(normalizedFile),
          contentType: imageMimeType,
        })
      } catch {
        toast.error(
          t("chat.imageReadFailed", {
            name: filename,
          }),
        )
      }
      continue
    }

    if (!isSupportedTextFile(file)) {
      toast.error(
        t("chat.invalidFile", {
          name: filename,
        }),
      )
      continue
    }

    if (file.size > MAX_CHAT_TEXT_FILE_SIZE_BYTES) {
      toast.error(
        t("chat.fileTooLarge", {
          name: filename,
          size: MAX_CHAT_TEXT_FILE_SIZE_LABEL,
        }),
      )
      continue
    }

    try {
      let text = await readFileAsText(file)
      if (text.trim().length === 0) {
        toast.error(
          t("chat.invalidFile", {
            name: filename,
          }),
        )
        continue
      }
      if (text.length > MAX_INLINE_FILE_CHARS) {
        text = `${text.slice(0, MAX_INLINE_FILE_CHARS)}\n\n${MAX_INLINE_FILE_TEXT_NOTE}`
        toast.warning(
          t("chat.fileTruncated", {
            name: filename,
          }),
        )
      }

      nextAttachments.push({
        type: "file",
        filename,
        url: "",
        contentType: file.type.trim() || "text/plain",
        text,
      })
    } catch {
      toast.error(
        t("chat.fileReadFailed", {
          name: filename,
        }),
      )
    }
  }

  return nextAttachments
}
