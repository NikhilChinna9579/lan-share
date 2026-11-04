package com.nikhil.lanshare

import android.content.ContentValues
import android.content.Context
import android.net.Uri
import android.provider.MediaStore
import android.webkit.MimeTypeMap

/**
 * Where received files go on the phone. Uses MediaStore, so no storage permission is
 * needed and files show up in Files, Gallery and Downloads like any other download.
 */
object Inbox {
    /** Folder keys we offer to senders (PROTOCOL.md "Folders") and their labels. */
    val FOLDERS = listOf("downloads" to "Downloads", "documents" to "Documents", "pictures" to "Pictures")
    const val DEFAULT = "downloads"

    class Pending(val uri: Uri, val mime: String)

    /** Creates a hidden ("pending") entry to write into; visible only after [publish]. */
    fun create(ctx: Context, folderKey: String, name: String): Pending {
        val mime = mimeOf(name)
        val media = mime.startsWith("image/") || mime.startsWith("video/")
        val (collection, dir) = when {
            folderKey == "pictures" && mime.startsWith("image/") ->
                MediaStore.Images.Media.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY) to "Pictures/"
            folderKey == "pictures" && mime.startsWith("video/") ->
                MediaStore.Video.Media.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY) to "Pictures/"
            folderKey == "documents" && !media ->
                MediaStore.Files.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY) to "Documents/"
            else -> MediaStore.Downloads.EXTERNAL_CONTENT_URI to "Download/"
        }
        fun insert(c: Uri, d: String): Uri? = ctx.contentResolver.insert(c, ContentValues().apply {
            put(MediaStore.MediaColumns.DISPLAY_NAME, name)
            put(MediaStore.MediaColumns.MIME_TYPE, mime)
            put(MediaStore.MediaColumns.RELATIVE_PATH, d)
            put(MediaStore.MediaColumns.IS_PENDING, 1)
        })
        // Some phones refuse certain types in certain folders; Downloads takes anything.
        val uri = runCatching { insert(collection, dir) }.getOrNull()
            ?: insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, "Download/")
            ?: error("Couldn't create $name")
        return Pending(uri, mime)
    }

    /** Makes the file visible and returns the name it was saved under (may get " (1)"). */
    fun publish(ctx: Context, p: Pending): String {
        ctx.contentResolver.update(p.uri, ContentValues().apply { put(MediaStore.MediaColumns.IS_PENDING, 0) }, null, null)
        ctx.contentResolver.query(p.uri, arrayOf(MediaStore.MediaColumns.DISPLAY_NAME), null, null, null)?.use {
            if (it.moveToFirst()) return it.getString(0)
        }
        return p.uri.lastPathSegment ?: "file"
    }

    fun discard(ctx: Context, p: Pending) {
        runCatching { ctx.contentResolver.delete(p.uri, null, null) }
    }

    /** Strips anything that could act as a path, and control characters. Null if nothing is left. */
    fun sanitize(name: String): String? {
        val base = name.substringAfterLast('/').substringAfterLast('\\')
        val clean = base.filter { !it.isISOControl() && it !in "<>:\"|?*" }.trim().trimEnd('.', ' ')
        return clean.takeIf { it.isNotEmpty() && it != "." && it != ".." }?.take(200)
    }

    fun mimeOf(name: String): String =
        MimeTypeMap.getSingleton().getMimeTypeFromExtension(name.substringAfterLast('.', "").lowercase())
            ?: "application/octet-stream"
}
