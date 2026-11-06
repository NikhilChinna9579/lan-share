package com.nikhil.lanshare

import android.content.ContentUris
import android.content.Context
import android.net.Uri
import android.provider.MediaStore
import android.util.Log
import java.util.UUID
import java.util.concurrent.Executors

/**
 * Sending and getting files, shared by the home screen and the assistant. Progress and
 * results go to [Hub], so whichever screen is open shows them live.
 */
object Actions {
    private const val TAG = "LanShare"
    val worker = Executors.newCachedThreadPool()

    fun send(ctx: Context, d: Device, folder: Client.Folder, path: String, where: String,
             items: List<Client.Outgoing>, explain: (Throwable) -> String) {
        val app = ctx.applicationContext
        val id = UUID.randomUUID().toString()
        val total = items.sumOf { it.size }
        val what = items.singleOrNull()?.name ?: plural(items.size, "file")
        Hub.transfer(id, false, d.name) {
            it.total = total; it.name = items.first().name; it.count = items.size; it.index = 1; it.folder = where
            it.cancel = { it.cancelled = true } // until connected; then it also cuts the connection
        }
        worker.execute {
            val result = runCatching {
                Client.send(DeviceStore.get(app, d.id) ?: d, deviceName(app), folder.key, path, items,
                    onConnected = { conn ->
                        Hub.transfer(id, false, d.name) { t -> t.cancel = { t.cancelled = true; conn.abort() } }
                        if (Hub.get(id)?.cancelled == true) conn.abort()
                    }) { sent, _, index ->
                    Hub.transfer(id, false, d.name) { t -> t.sent = sent; t.index = index + 1; t.name = items[index].name }
                }
            }
            val again = { send(app, d, folder, path, where, items, explain) }
            Log.i(TAG, "send to ${d.name}: " + result.map { "ok" }.getOrElse { "failed: $it" })
            result.onSuccess {
                Hub.transfer(id, false, d.name) { t -> t.state = Hub.State.DONE; t.sent = total; t.files = items.size; t.cancel = null }
                val took = Hub.get(id)?.elapsed ?: 0
                Hub.add("sent", "Sent $what to ${d.name}",
                    "In $where · ${formatSize(total)} in ${formatDuration(took)} (${formatSize(total * 1000 / maxOf(took, 1))}/s)")
            }.onFailure { e ->
                if (Hub.get(id)?.cancelled == true) {
                    Hub.transfer(id, false, d.name) { t -> t.state = Hub.State.CANCELLED; t.message = "Cancelled. Nothing was kept on ${d.name}."; t.retry = again; t.cancel = null }
                    Hub.add("info", "Cancelled sending $what to ${d.name}")
                } else {
                    val msg = explain(e)
                    Hub.transfer(id, false, d.name) { t -> t.state = Hub.State.ERROR; t.message = msg; t.retry = again; t.cancel = null }
                    Hub.add("error", "Couldn't send $what to ${d.name}", msg)
                }
            }
        }
    }

    fun fetch(ctx: Context, d: Device, folder: Client.Folder, path: String, names: List<String>, total: Long,
              explain: (Throwable) -> String) {
        val app = ctx.applicationContext
        val id = UUID.randomUUID().toString()
        val what = names.singleOrNull() ?: plural(names.size, "file")
        Hub.transfer(id, true, d.name) {
            it.total = total; it.name = names.first(); it.count = names.size; it.index = 1; it.folder = "Downloads"
            it.cancel = { it.cancelled = true }
        }
        worker.execute {
            val result = runCatching {
                Client.fetch(app, DeviceStore.get(app, d.id) ?: d, deviceName(app), folder.key, path, names,
                    onConnected = { conn ->
                        Hub.transfer(id, true, d.name) { t -> t.cancel = { t.cancelled = true; conn.abort() } }
                        if (Hub.get(id)?.cancelled == true) conn.abort()
                    }) { got, all, index ->
                    Hub.transfer(id, true, d.name) { t -> t.sent = got; t.total = all; t.index = index + 1; t.name = names[index] }
                }
            }
            val again = { fetch(app, d, folder, path, names, total, explain) }
            Log.i(TAG, "fetch from ${d.name}: " + result.map { "ok ${it.size} files" }.getOrElse { "failed: $it" })
            result.onSuccess { saved ->
                val where = saved.lastOrNull()?.where ?: "Downloads"
                Hub.transfer(id, true, d.name) { t -> t.state = Hub.State.DONE; t.sent = t.total; t.files = saved.size; t.cancel = null; t.folder = where }
                val took = Hub.get(id)?.elapsed ?: 0
                Hub.add("received", "Got $what from ${d.name}", "${formatSize(Hub.get(id)?.total ?: total)} in ${formatDuration(took)} · in $where")
                ReceiveService.notifyReceived(app, PhoneServer.Received(d.name, where, saved, took))
            }.onFailure { e ->
                if (Hub.get(id)?.cancelled == true) {
                    Hub.transfer(id, true, d.name) { t -> t.state = Hub.State.CANCELLED; t.message = "Cancelled. Nothing was kept."; t.retry = again; t.cancel = null }
                    Hub.add("info", "Cancelled getting $what from ${d.name}")
                } else {
                    val msg = explain(e)
                    Hub.transfer(id, true, d.name) { t -> t.state = Hub.State.ERROR; t.message = msg; t.retry = again; t.cancel = null }
                    Hub.add("error", "Couldn't get $what from ${d.name}", msg)
                }
            }
        }
    }

    // ------------------------------------------------------------- tap to open

    /**
     * Opens a laptop's file on this phone: from the cache if it's there and unchanged,
     * otherwise fetched into the cache first (with the usual progress card and Cancel).
     * [ready] gets the file to show, or null if it couldn't be fetched.
     */
    fun openRemote(ctx: Context, d: Device, folder: Client.Folder, path: String, f: Client.RemoteFile,
                   explain: (Throwable) -> String, ready: (java.io.File?) -> Unit) {
        val app = ctx.applicationContext
        val dest = RemoteCache.fileFor(app, d, folder.key, path, f.name)
        if (RemoteCache.fresh(dest, f.size, f.modified)) {
            Log.i(TAG, "open: ${f.name} from the cache")
            dest.setLastModified(System.currentTimeMillis()) // recently used: kept longer
            ready(dest)
            return
        }
        val id = UUID.randomUUID().toString()
        Hub.transfer(id, true, d.name) {
            it.total = f.size; it.name = f.name; it.count = 1; it.index = 1; it.folder = "Opening"
            it.cancel = { it.cancelled = true }
        }
        worker.execute {
            val r = runCatching {
                Client.fetchToFile(app, DeviceStore.get(app, d.id) ?: d, deviceName(app), folder.key, path, f.name, dest,
                    onConnected = { conn ->
                        Hub.transfer(id, true, d.name) { t -> t.cancel = { t.cancelled = true; conn.abort() } }
                        if (Hub.get(id)?.cancelled == true) conn.abort()
                    }) { got, all, _ -> Hub.transfer(id, true, d.name) { t -> t.sent = got; t.total = all } }
                RemoteCache.remember(dest, f.modified)
            }
            Log.i(TAG, "open: ${f.name} from ${d.name}: " + r.map { "fetched ${dest.length()} bytes" }.getOrElse { "failed: $it" })
            r.onSuccess {
                Hub.transfer(id, true, d.name) { t -> t.state = Hub.State.DONE; t.sent = t.total; t.files = 1; t.cancel = null; t.folder = "Opened" }
            }.onFailure { e ->
                val cancelled = Hub.get(id)?.cancelled == true
                Hub.transfer(id, true, d.name) { t ->
                    t.state = if (cancelled) Hub.State.CANCELLED else Hub.State.ERROR
                    t.message = if (cancelled) "Cancelled." else explain(e); t.cancel = null
                }
            }
            android.os.Handler(android.os.Looper.getMainLooper()).post { ready(if (r.isSuccess) dest else null) }
            worker.execute { RemoteCache.clean(app) }
        }
    }

    /** "Save" for a file already opened (in the cache): copied here, no second download. */
    fun saveFromCache(ctx: Context, d: Device, src: java.io.File, name: String) {
        val app = ctx.applicationContext
        worker.execute {
            val r = runCatching {
                val inc = SharedFolders.create(app, SharedFolders.DEFAULT, "", name)
                try {
                    src.inputStream().use { it.copyTo(inc.out) }
                    inc.finish() to inc.savedIn
                } catch (e: Exception) { inc.discard(); throw e }
            }
            Log.i(TAG, "save from cache: $name " + r.map { "ok" }.getOrElse { "failed: $it" })
            r.onSuccess { (saved, where) ->
                Hub.add("received", "Got $saved from ${d.name}", "${formatSize(src.length())} · in $where (already opened, no download)")
            }.onFailure { Hub.add("error", "Couldn't save $name", it.message ?: "") }
        }
    }

    // ------------------------------------------------------------- shared clipboard

    /** Sends text to the laptop's clipboard. [done] gets null on success, or what went wrong. */
    fun sendClip(ctx: Context, d: Device, text: String, explain: (Throwable) -> String, quiet: Boolean = false,
                 auto: Boolean = false, done: (String?) -> Unit = {}) {
        val app = ctx.applicationContext
        worker.execute {
            val r = runCatching { Client.pushClip(DeviceStore.get(app, d.id) ?: d, deviceName(app), text, auto) }
            Log.i(TAG, "clipboard sent to ${d.name}: " + r.map { "${text.length} chars" }.getOrElse { "failed: $it" })
            r.onSuccess { Hub.add("sent", "Copied to ${d.name}'s clipboard", Clip.oneLine(text)) }
                .onFailure { if (!quiet) Hub.add("error", "Couldn't send the clipboard to ${d.name}", explain(it)) }
            done(r.exceptionOrNull()?.let(explain))
        }
    }

    /** Copies the laptop's clipboard onto this phone's. [done] gets null on success, or why not. */
    fun pasteFrom(ctx: Context, d: Device, explain: (Throwable) -> String, done: (String?) -> Unit = {}) {
        val app = ctx.applicationContext
        worker.execute {
            val r = runCatching { Client.requestClip(DeviceStore.get(app, d.id) ?: d, deviceName(app)) }
            r.onSuccess { text ->
                Clip.write(app, text, d.name, notify = false)
                Log.i(TAG, "clipboard pasted from ${d.name}: ${text.length} chars")
                Hub.add("received", "Copied from ${d.name}'s clipboard", Clip.oneLine(text))
            }.onFailure { Log.i(TAG, "paste from ${d.name} failed: $it") }
            done(r.exceptionOrNull()?.let { if (it is IllegalStateException) it.message else explain(it) })
        }
    }

    // ------------------------------------------------------------- finding "this file"

    /** The newest photo, screenshot, video or download on this phone; null if none (or no permission). */
    fun latest(ctx: Context, what: Commands.What): Uri? = latestWithDate(ctx, what)?.first

    /** The newest photo/screenshot/…, and when it was added (ms). */
    fun latestWithDate(ctx: Context, what: Commands.What): Pair<Uri, Long>? {
        val vol = MediaStore.VOLUME_EXTERNAL
        val (collection, sel) = when (what) {
            Commands.What.PHOTO -> MediaStore.Images.Media.getContentUri(vol) to "${MediaStore.MediaColumns.RELATIVE_PATH} NOT LIKE '%Screenshots%'"
            Commands.What.SCREENSHOT -> MediaStore.Images.Media.getContentUri(vol) to "${MediaStore.MediaColumns.RELATIVE_PATH} LIKE '%Screenshots%'"
            Commands.What.VIDEO -> MediaStore.Video.Media.getContentUri(vol) to null
            Commands.What.DOWNLOAD -> MediaStore.Downloads.getContentUri(vol) to null
            else -> return null
        }
        return runCatching {
            ctx.contentResolver.query(collection, arrayOf(MediaStore.MediaColumns._ID, MediaStore.MediaColumns.DATE_ADDED), sel, null,
                "${MediaStore.MediaColumns.DATE_ADDED} DESC")?.use { c ->
                if (c.moveToFirst()) ContentUris.withAppendedId(collection, c.getLong(0)) to c.getLong(1) * 1000 else null
            }
        }.getOrNull()
    }

    /** The phone's file best matching a spoken name ("report", "IMG 2041"); null if nothing fits. */
    fun findOnPhone(ctx: Context, said: String): Uri? {
        val base = MediaStore.Files.getContentUri(MediaStore.VOLUME_EXTERNAL)
        val word = Commands.normalize(said).substringBeforeLast('.').split(' ', '_', '-').maxByOrNull { it.length } ?: return null
        var best: Uri? = null
        var bestScore = 0
        runCatching {
            ctx.contentResolver.query(base, arrayOf(MediaStore.MediaColumns._ID, MediaStore.MediaColumns.DISPLAY_NAME),
                "${MediaStore.MediaColumns.DISPLAY_NAME} LIKE ?", arrayOf("%$word%"), "${MediaStore.MediaColumns.DATE_MODIFIED} DESC")?.use { c ->
                var n = 0
                while (c.moveToNext() && n++ < 500) {
                    val score = Commands.nameScore(c.getString(1) ?: continue, said)
                    if (score > bestScore) { bestScore = score; best = ContentUris.withAppendedId(base, c.getLong(0)) }
                }
            }
        }
        return best
    }
}
