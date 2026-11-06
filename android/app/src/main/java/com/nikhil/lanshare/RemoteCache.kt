package com.nikhil.lanshare

import android.content.ContentProvider
import android.content.ContentValues
import android.content.Context
import android.database.Cursor
import android.database.MatrixCursor
import android.net.Uri
import android.os.ParcelFileDescriptor
import android.provider.OpenableColumns
import java.io.File

/**
 * Files opened from another device (tap to open): fetched into this app's cache, opened in
 * the phone's normal viewer, and deleted after a day (or when the cache passes 500 MB).
 * Opening the same, unchanged file again needs no download.
 */
object RemoteCache {
    private const val MAX_BYTES = 500L shl 20
    private const val MAX_AGE = 24 * 3600_000L

    fun root(ctx: Context) = File(ctx.cacheDir, "remote")

    /** Where a laptop's file is kept here. */
    fun fileFor(ctx: Context, d: Device, folder: String, path: String, name: String): File {
        val parts = listOf(d.id.take(12), SharedFolders.sanitize(folder) ?: "f") + (SharedFolders.cleanPath(path) ?: emptyList())
        return File(File(root(ctx), parts.joinToString("/")), SharedFolders.sanitize(name) ?: "file")
    }

    private fun meta(f: File) = File(f.parentFile, ".${f.name}.meta")

    /** Already here, and the same size and date as on the laptop? */
    fun fresh(f: File, size: Long, modified: Long): Boolean =
        f.isFile && f.length() == size && runCatching { meta(f).readText().trim().toLong() == modified }.getOrDefault(false)

    fun remember(f: File, modified: Long) { runCatching { meta(f).writeText(modified.toString()) } }

    /** Deletes what's older than a day, then the oldest until under 500 MB. */
    fun clean(ctx: Context) {
        val now = System.currentTimeMillis()
        val files = root(ctx).walkBottomUp().filter { it.isFile && !it.name.startsWith(".") }.toMutableList()
        files.filter { now - it.lastModified() > MAX_AGE }.forEach { it.delete(); meta(it).delete() }
        val left = files.filter { it.exists() }.sortedBy { it.lastModified() }.toMutableList()
        var total = left.sumOf { it.length() }
        while (total > MAX_BYTES && left.isNotEmpty()) {
            val f = left.removeAt(0)
            total -= f.length(); f.delete(); meta(f).delete()
        }
        root(ctx).walkBottomUp().filter { it.isFile && it.name.startsWith(".") && it.name.endsWith(".meta") &&
            !File(it.parentFile, it.name.removePrefix(".").removeSuffix(".meta")).exists() }.forEach { it.delete() }
        root(ctx).walkBottomUp().filter { it.isDirectory && it != root(ctx) && (it.list()?.isEmpty() == true) }.forEach { it.delete() }
    }

    /** A content:// link the viewer app may read (only for files in this cache). */
    fun uriFor(ctx: Context, f: File): Uri {
        val rel = f.relativeTo(root(ctx)).invariantSeparatorsPath
        return Uri.Builder().scheme("content").authority("${ctx.packageName}.cache")
            .apply { rel.split('/').forEach { appendPath(it) } }.build()
    }
}

/** Lets the viewer app read a file from [RemoteCache] (read-only, only with our permission grant). */
class CacheProvider : ContentProvider() {
    override fun onCreate() = true

    private fun file(uri: Uri): File? {
        val ctx = context ?: return null
        val root = RemoteCache.root(ctx).canonicalFile
        val f = File(root, uri.pathSegments.joinToString("/")).canonicalFile
        return f.takeIf { it.path.startsWith(root.path + File.separator) && it.isFile }
    }

    override fun openFile(uri: Uri, mode: String): ParcelFileDescriptor {
        if (mode != "r") throw SecurityException("read only")
        val f = file(uri) ?: throw java.io.FileNotFoundException(uri.toString())
        return ParcelFileDescriptor.open(f, ParcelFileDescriptor.MODE_READ_ONLY)
    }

    override fun getType(uri: Uri): String = SharedFolders.mimeOf(uri.lastPathSegment ?: "")

    override fun query(uri: Uri, projection: Array<out String>?, selection: String?, args: Array<out String>?, sort: String?): Cursor {
        val f = file(uri)
        val cols = projection ?: arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE)
        return MatrixCursor(cols).apply {
            if (f != null) addRow(cols.map { if (it == OpenableColumns.SIZE) f.length() else if (it == OpenableColumns.DISPLAY_NAME) f.name else null })
        }
    }

    override fun insert(uri: Uri, values: ContentValues?): Uri? = null
    override fun update(uri: Uri, values: ContentValues?, s: String?, a: Array<out String>?) = 0
    override fun delete(uri: Uri, s: String?, a: Array<out String>?) = 0
}
