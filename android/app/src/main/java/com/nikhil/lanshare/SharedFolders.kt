package com.nikhil.lanshare

import android.content.ContentValues
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.provider.DocumentsContract
import android.provider.MediaStore
import android.webkit.MimeTypeMap
import org.json.JSONArray
import org.json.JSONObject
import java.io.OutputStream
import java.security.MessageDigest

/**
 * The folders on this phone that paired laptops may save into (and, later, browse).
 * Nothing else is reachable, so system folders can never be touched.
 *
 * Two kinds:
 *  - Standard folders (Downloads, Documents, Pictures, Camera, Movies, Music), written
 *    through MediaStore. No storage permission needed; can be switched off.
 *  - Folders the user picks with Android's own folder picker ("Add a folder"). Android
 *    grants access to exactly that folder and its subfolders, which can be browsed.
 */
object SharedFolders {

    data class Root(val key: String, val label: String, val dir: String?, val tree: Uri?) {
        val builtin get() = tree == null
    }

    val STANDARD = listOf(
        Root("downloads", "Downloads", "Download/", null),
        Root("documents", "Documents", "Documents/", null),
        Root("pictures", "Pictures", "Pictures/", null),
        Root("camera", "Camera", "DCIM/Camera/", null),
        Root("movies", "Movies", "Movies/", null),
        Root("music", "Music", "Music/", null),
    )
    const val DEFAULT = "downloads"

    private fun prefs(ctx: Context) = ctx.getSharedPreferences("shared_folders", Context.MODE_PRIVATE)

    fun roots(ctx: Context): List<Root> {
        val off = prefs(ctx).getStringSet("off", emptySet())!!
        val trees = JSONArray(prefs(ctx).getString("trees", "[]"))
        return STANDARD.filter { it.key !in off } + (0 until trees.length()).map {
            val o = trees.getJSONObject(it)
            Root(o.getString("key"), o.getString("label"), null, Uri.parse(o.getString("uri")))
        }
    }

    fun isOn(ctx: Context, key: String) = key !in prefs(ctx).getStringSet("off", emptySet())!!

    fun setStandard(ctx: Context, key: String, on: Boolean) {
        val off = prefs(ctx).getStringSet("off", emptySet())!!.toMutableSet()
        if (on) off -= key else off += key
        prefs(ctx).edit().putStringSet("off", off).apply()
    }

    fun trees(ctx: Context): List<Root> = roots(ctx).filter { !it.builtin }

    /** Shares a folder picked with ACTION_OPEN_DOCUMENT_TREE and keeps access to it. */
    fun addTree(ctx: Context, uri: Uri): Root {
        ctx.contentResolver.takePersistableUriPermission(uri,
            Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_WRITE_URI_PERMISSION)
        val docId = DocumentsContract.getTreeDocumentId(uri)
        val label = queryName(ctx, DocumentsContract.buildDocumentUriUsingTree(uri, docId))
            ?: docId.substringAfterLast(':').substringAfterLast('/').ifEmpty { "Folder" }
        val key = "t-" + MessageDigest.getInstance("SHA-256").digest(uri.toString().toByteArray()).toHex().take(8)
        val arr = JSONArray(prefs(ctx).getString("trees", "[]"))
        if ((0 until arr.length()).none { arr.getJSONObject(it).getString("key") == key }) {
            arr.put(JSONObject().put("key", key).put("label", label).put("uri", uri.toString()))
            prefs(ctx).edit().putString("trees", arr.toString()).apply()
        }
        return Root(key, label, null, uri)
    }

    fun removeTree(ctx: Context, key: String) {
        val arr = JSONArray(prefs(ctx).getString("trees", "[]"))
        val keep = JSONArray()
        for (i in 0 until arr.length()) {
            val o = arr.getJSONObject(i)
            if (o.getString("key") == key) {
                runCatching {
                    ctx.contentResolver.releasePersistableUriPermission(Uri.parse(o.getString("uri")),
                        Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_WRITE_URI_PERMISSION)
                }
            } else {
                keep.put(o)
            }
        }
        prefs(ctx).edit().putString("trees", keep.toString()).apply()
    }

    // ------------------------------------------------------------- browsing

    class Listing(val dirs: List<String>, val files: List<Triple<String, Long, Long>>) // name, size, modified

    /**
     * Subfolders and files at [path] inside a shared folder. Standard folders are read
     * through MediaStore: photos, videos and music need the media permission; other
     * files show only if LAN Share saved them (Android hides other apps' files).
     */
    fun list(ctx: Context, key: String, path: String): Listing {
        val root = roots(ctx).firstOrNull { it.key == key } ?: throw IllegalArgumentException("bad_folder")
        val parts = cleanPath(path) ?: throw IllegalArgumentException("bad_folder")
        val dirs = sortedSetOf<String>(String.CASE_INSENSITIVE_ORDER)
        val files = mutableListOf<Triple<String, Long, Long>>()
        if (root.tree == null) {
            val dir = root.dir!! + parts.joinToString("") { "$it/" }
            media(ctx, dir, exact = false) { _, name, rel, size, modified ->
                if (rel.equals(dir, ignoreCase = true)) files += Triple(name, size, modified)
                else rel.substring(dir.length).substringBefore('/').takeIf { it.isNotEmpty() && !it.startsWith(".") }?.let { dirs += it }
            }
            // Folders on disk (also empty ones), and ones just made that are waiting for a file.
            @Suppress("DEPRECATION")
            runCatching { java.io.File(android.os.Environment.getExternalStorageDirectory(), dir).listFiles() }.getOrNull()
                ?.filter { it.isDirectory && !it.name.startsWith(".") }?.forEach { dirs += it.name }
            pendingDirs.filter { it.startsWith(dir, ignoreCase = true) && it.length > dir.length }
                .forEach { dirs += it.substring(dir.length).substringBefore('/') }
            return Listing(dirs.toList(), files.sortedByDescending { it.third }.take(2000))
        }
        val dirId = resolveDir(ctx, root.tree, path) ?: throw IllegalArgumentException("bad_folder")
        children(ctx, root.tree, dirId) { _, name, mime, size, modified ->
            if (name.startsWith(".")) return@children // hidden
            if (mime == DocumentsContract.Document.MIME_TYPE_DIR) dirs += name else files += Triple(name, size, modified)
        }
        return Listing(dirs.toList(), files.sortedByDescending { it.third }.take(2000))
    }

    /** A file in a shared folder, ready to send; null if it isn't there. */
    fun open(ctx: Context, key: String, path: String, name: String): Client.Outgoing? {
        val root = roots(ctx).firstOrNull { it.key == key } ?: return null
        val parts = cleanPath(path) ?: return null
        if (root.tree == null) {
            val dir = root.dir!! + parts.joinToString("") { "$it/" }
            var found: Client.Outgoing? = null
            media(ctx, dir, exact = true, name = name) { uri, n, _, size, _ ->
                if (found == null && n == name) found = Client.Outgoing(n, size) {
                    ctx.contentResolver.openInputStream(uri) ?: error("Can't open $n")
                }
            }
            return found
        }
        val dirId = resolveDir(ctx, root.tree, path) ?: return null
        var found: Client.Outgoing? = null
        children(ctx, root.tree, dirId) { id, n, mime, size, _ ->
            if (found == null && n == name && mime != DocumentsContract.Document.MIME_TYPE_DIR) {
                val uri = DocumentsContract.buildDocumentUriUsingTree(root.tree, id)
                found = Client.Outgoing(n, size) { ctx.contentResolver.openInputStream(uri) ?: error("Can't open $n") }
            }
        }
        return found
    }

    /** Can laptops see this phone's photos, videos and music? */
    fun canReadMedia(ctx: Context): Boolean = mediaPermissions().all {
        ctx.checkSelfPermission(it) == android.content.pm.PackageManager.PERMISSION_GRANTED
    }

    fun mediaPermissions(): Array<String> = if (android.os.Build.VERSION.SDK_INT >= 33) arrayOf(
        android.Manifest.permission.READ_MEDIA_IMAGES, android.Manifest.permission.READ_MEDIA_VIDEO,
        android.Manifest.permission.READ_MEDIA_AUDIO,
    ) else arrayOf(android.Manifest.permission.READ_EXTERNAL_STORAGE)

    /** Visible MediaStore items under [dir] (or exactly in it): (uri, name, relative path, size, modified ms). */
    private fun media(ctx: Context, dir: String, exact: Boolean, name: String? = null,
                      each: (Uri, String, String, Long, Long) -> Unit) {
        val base = MediaStore.Files.getContentUri(MediaStore.VOLUME_EXTERNAL)
        val cols = arrayOf(MediaStore.MediaColumns._ID, MediaStore.MediaColumns.DISPLAY_NAME,
            MediaStore.MediaColumns.RELATIVE_PATH, MediaStore.MediaColumns.SIZE, MediaStore.MediaColumns.DATE_MODIFIED)
        val sel = StringBuilder(if (exact) "${MediaStore.MediaColumns.RELATIVE_PATH} = ?" else "${MediaStore.MediaColumns.RELATIVE_PATH} LIKE ?")
        val args = mutableListOf(if (exact) dir else dir.replace("%", "\\%").replace("_", "\\_") + "%")
        if (!exact) sel.append(" ESCAPE '\\'")
        sel.append(" AND ${MediaStore.MediaColumns.IS_PENDING} = 0")
        if (name != null) { sel.append(" AND ${MediaStore.MediaColumns.DISPLAY_NAME} = ?"); args += name }
        runCatching {
            ctx.contentResolver.query(base, cols, sel.toString(), args.toTypedArray(), null)?.use { c ->
                var n = 0
                while (c.moveToNext() && n++ < 5000) {
                    val nm = c.getString(1) ?: continue
                    if (nm.startsWith(".")) continue
                    each(android.content.ContentUris.withAppendedId(base, c.getLong(0)), nm, c.getString(2) ?: "",
                        if (c.isNull(3)) 0 else c.getLong(3), if (c.isNull(4)) 0 else c.getLong(4) * 1000)
                }
            }
        }.onFailure { android.util.Log.w("LanShare", "media query failed: $it") }
    }

    // ------------------------------------------------------------- saving

    /** A file being received: write to [out], then [finish] or [discard]. */
    class Incoming(val uri: Uri, val mime: String, val out: OutputStream, val savedIn: String,
                   private val ctx: Context, private val pending: Boolean) {
        fun finish(): String {
            out.close()
            if (pending) {
                ctx.contentResolver.update(uri, ContentValues().apply { put(MediaStore.MediaColumns.IS_PENDING, 0) }, null, null)
            }
            return queryName(ctx, uri) ?: "file"
        }

        fun discard() {
            runCatching { out.close() }
            runCatching {
                if (pending) ctx.contentResolver.delete(uri, null, null) else DocumentsContract.deleteDocument(ctx.contentResolver, uri)
            }
        }
    }

    fun create(ctx: Context, key: String, path: String, name: String): Incoming {
        val root = roots(ctx).firstOrNull { it.key == key } ?: throw IllegalArgumentException("bad_folder")
        val mime = mimeOf(name)
        val parts = cleanPath(path) ?: throw IllegalArgumentException("bad_folder")
        if (root.tree != null) {
            val dirId = resolveDir(ctx, root.tree, path, create = true) ?: throw IllegalArgumentException("bad_folder")
            val doc = DocumentsContract.createDocument(ctx.contentResolver,
                DocumentsContract.buildDocumentUriUsingTree(root.tree, dirId), mime, name)
                ?: error("Couldn't create $name")
            val out = ctx.contentResolver.openOutputStream(doc) ?: error("Couldn't write $name")
            return Incoming(doc, mime, out, (listOf(root.label) + parts).joinToString("/"), ctx, pending = false)
        }
        // Standard folder via MediaStore: each kind of file has its own collection, and
        // each collection only accepts certain top folders. Anything that doesn't fit
        // goes to Downloads, which accepts everything.
        val dir = root.dir!! + parts.joinToString("") { "$it/" }
        val top = root.dir.substringBefore('/')
        val volume = MediaStore.VOLUME_EXTERNAL_PRIMARY
        val collection = when {
            top == "Download" -> MediaStore.Downloads.getContentUri(volume)
            top == "Documents" -> MediaStore.Files.getContentUri(volume)
            mime.startsWith("image/") && top in setOf("Pictures", "DCIM") -> MediaStore.Images.Media.getContentUri(volume)
            mime.startsWith("video/") && top in setOf("Pictures", "DCIM", "Movies") -> MediaStore.Video.Media.getContentUri(volume)
            mime.startsWith("audio/") && top == "Music" -> MediaStore.Audio.Media.getContentUri(volume)
            else -> null
        }
        fun insert(c: android.net.Uri, d: String) = runCatching {
            ctx.contentResolver.insert(c, ContentValues().apply {
                put(MediaStore.MediaColumns.DISPLAY_NAME, name)
                put(MediaStore.MediaColumns.MIME_TYPE, mime)
                put(MediaStore.MediaColumns.RELATIVE_PATH, d)
                put(MediaStore.MediaColumns.IS_PENDING, 1)
            })
        }.getOrNull()
        var savedIn = (listOf(root.label) + parts).joinToString("/")
        val uri = collection?.let { insert(it, dir) } ?: run {
            savedIn = "Downloads"
            insert(MediaStore.Downloads.getContentUri(volume), "Download/")
        } ?: error("Couldn't create $name")
        val out = ctx.contentResolver.openOutputStream(uri) ?: error("Couldn't write $name")
        return Incoming(uri, mime, out, savedIn, ctx, pending = true)
    }

    // ------------------------------------------------------------- helpers

    /** Splits "Work/2026" into safe parts; null if it tries to escape (".."). */
    fun cleanPath(path: String): List<String>? {
        val parts = path.replace('\\', '/').split('/').filter { it.isNotEmpty() && it != "." }
        return if (parts.any { it == ".." || it.any { c -> c.isISOControl() } }) null else parts
    }

    /** The document id of [path] inside a picked folder; with [create], missing folders are made. */
    private fun resolveDir(ctx: Context, tree: Uri, path: String, create: Boolean = false): String? {
        var id = DocumentsContract.getTreeDocumentId(tree)
        for (part in cleanPath(path) ?: return null) {
            var next: String? = null
            children(ctx, tree, id) { childId, name, mime, _, _ ->
                if (next == null && name == part && mime == DocumentsContract.Document.MIME_TYPE_DIR) next = childId
            }
            if (next == null && create) {
                next = DocumentsContract.createDocument(ctx.contentResolver, DocumentsContract.buildDocumentUriUsingTree(tree, id),
                    DocumentsContract.Document.MIME_TYPE_DIR, part)?.let { DocumentsContract.getDocumentId(it) }
            }
            id = next ?: return null
        }
        return id
    }

    // Empty folders made in a standard folder that Android won't let us create on disk
    // yet: they appear when the first file is saved into them (MediaStore makes them).
    private val pendingDirs = java.util.concurrent.ConcurrentHashMap.newKeySet<String>()

    /** Makes a subfolder [name] inside (key, path), e.g. from a laptop's "New folder". */
    fun mkdir(ctx: Context, key: String, path: String, name: String): String {
        val root = roots(ctx).firstOrNull { it.key == key } ?: throw IllegalArgumentException("not a shared folder")
        val parts = cleanPath(path) ?: throw IllegalArgumentException("not a shared folder")
        val clean = sanitize(name) ?: throw IllegalArgumentException("bad folder name")
        if (root.tree != null) {
            resolveDir(ctx, root.tree, (parts + clean).joinToString("/"), create = true) ?: error("Couldn't make $clean")
        } else {
            val rel = root.dir!! + parts.joinToString("") { "$it/" } + clean + "/"
            @Suppress("DEPRECATION")
            val made = runCatching { java.io.File(android.os.Environment.getExternalStorageDirectory(), rel).let { it.isDirectory || it.mkdirs() } }.getOrDefault(false)
            if (!made) pendingDirs += rel
        }
        return clean
    }

    private fun children(ctx: Context, tree: Uri, parentId: String, each: (String, String, String, Long, Long) -> Unit) {
        val uri = DocumentsContract.buildChildDocumentsUriUsingTree(tree, parentId)
        val cols = arrayOf(
            DocumentsContract.Document.COLUMN_DOCUMENT_ID, DocumentsContract.Document.COLUMN_DISPLAY_NAME,
            DocumentsContract.Document.COLUMN_MIME_TYPE, DocumentsContract.Document.COLUMN_SIZE,
            DocumentsContract.Document.COLUMN_LAST_MODIFIED,
        )
        ctx.contentResolver.query(uri, cols, null, null, null)?.use { c ->
            while (c.moveToNext()) {
                each(c.getString(0), c.getString(1) ?: "", c.getString(2) ?: "",
                    if (c.isNull(3)) 0 else c.getLong(3), if (c.isNull(4)) 0 else c.getLong(4))
            }
        }
    }

    fun sanitize(name: String): String? {
        val base = name.substringAfterLast('/').substringAfterLast('\\')
        val clean = base.filter { !it.isISOControl() && it !in "<>:\"|?*" }.trim().trimEnd('.', ' ')
        return clean.takeIf { it.isNotEmpty() && it != "." && it != ".." }?.take(200)
    }

    fun mimeOf(name: String): String =
        MimeTypeMap.getSingleton().getMimeTypeFromExtension(name.substringAfterLast('.', "").lowercase())
            ?: "application/octet-stream"

    private fun queryName(ctx: Context, uri: Uri): String? = runCatching {
        ctx.contentResolver.query(uri, arrayOf(android.provider.OpenableColumns.DISPLAY_NAME), null, null, null)?.use {
            if (it.moveToFirst()) it.getString(0) else null
        }
    }.getOrNull()
}
