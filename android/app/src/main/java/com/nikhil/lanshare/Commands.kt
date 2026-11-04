package com.nikhil.lanshare

/**
 * Understands what was said (or typed) to the assistant. Rule-based, so it works
 * offline and instantly; plain Kotlin with no Android parts, so it's unit-tested.
 *
 *   "send this to my laptop"                    → Send(THIS, device = the laptop)
 *   "send the last photo to desktop in documents" → Send(PHOTO, folder = "documents")
 *   "share report dot pdf with the office pc"   → Send(NAMED "report.pdf")
 *   "get budget.xlsx from my laptop"            → Get("budget.xlsx")
 *   "is my laptop online"                       → Status
 */
object Commands {
    enum class What { THIS, PHOTO, SCREENSHOT, VIDEO, DOWNLOAD, NAMED }

    sealed class Command {
        /** [device] is one of the names passed to [parse], or null if none was named (or it's unclear). */
        data class Send(val what: What, val name: String?, val device: String?, val folder: String?) : Command()
        data class Get(val name: String, val device: String?) : Command()
        data class Status(val device: String?) : Command()
        object Unknown : Command()
    }

    private val sendVerbs = setOf("send", "share", "transfer", "move", "put", "upload", "throw", "push", "copy")
    private val getVerbs = setOf("get", "fetch", "download", "bring", "grab", "pull", "open")
    private val generic = setOf("laptop", "computer", "pc", "desktop", "windows", "notebook", "machine")
    private val filler = setOf("the", "my", "a", "an", "please", "file", "called", "named", "last", "latest", "recent",
        "most", "newest", "folder", "directory", "to", "in", "into", "on", "s", "of", "me", "for", "it", "just", "now")

    fun parse(text: String, devices: List<String>): Command {
        val t = normalize(text)
        if (t.isEmpty()) return Command.Unknown
        val words = t.split(' ')
        // Skip polite openers: "can you", "please", "hey", "could you".
        val start = words.indexOfFirst { it in sendVerbs || it in getVerbs }
        if (start < 0) {
            return if (Regex("\\b(online|offline|status|connected|reachable|there)\\b").containsMatchIn(t))
                Command.Status(findDevice(t, devices)) else Command.Unknown
        }
        val verb = words[start]
        val rest = words.drop(start + 1).joinToString(" ")

        if (verb in getVerbs && (" from " in " $rest " || verb != "copy")) {
            val (obj, from) = splitAt(rest, listOf("from", "off", "on"))
            val name = cleanName(obj) ?: return Command.Unknown
            return Command.Get(name, findDevice(from ?: "", devices))
        }

        // Send: "<object> to|with <target>"
        val (obj, target) = splitAt(rest, listOf("to", "with", "onto", "over to"))
        val what = whatOf(obj)
        val name = if (what == What.NAMED) cleanName(obj) ?: return Command.Unknown else null
        var device: String? = null
        var folder: String? = null
        if (target != null) {
            // "desktop in documents", "laptop's downloads folder", "documents on my laptop"
            val (a, b) = splitAt(target, listOf("in", "into", "inside", "under", "on"))
            device = findDevice(a, devices) ?: b?.let { findDevice(it, devices) }
            val folderPart = when {
                b == null -> stripDevice(a, devices)
                findDevice(a, devices) != null -> b
                else -> stripDevice(a, devices)
            }
            folder = folderPart.split(' ').filter { it.isNotEmpty() && it !in filler }.joinToString(" ").ifEmpty { null }
        }
        return Command.Send(what, name, device, folder)
    }

    /** Lower case, "dot" → ".", apostrophes and most punctuation removed. */
    fun normalize(text: String): String = text.lowercase()
        .replace(Regex("\\s+dot\\s+"), ".")
        .replace("'s ", " s ").replace("’s ", " s ")
        .replace(Regex("[^\\p{L}\\p{N}._\\- ]"), " ")
        .replace(Regex("\\s+"), " ").trim().trimEnd('.')

    private fun splitAt(s: String, seps: List<String>): Pair<String, String?> {
        val padded = " $s "
        var best = -1
        var len = 0
        for (sep in seps) {
            val i = padded.indexOf(" $sep ")
            if (i >= 0 && (best < 0 || i < best)) { best = i; len = sep.length + 2 }
        }
        if (best < 0) return s to null
        return padded.substring(0, best).trim() to padded.substring(best + len).trim()
    }

    private fun whatOf(obj: String): What {
        val w = obj.split(' ').toSet()
        return when {
            w.any { it in setOf("this", "it", "that", "these", "those") } && w.none { '.' in it } -> What.THIS
            w.any { it.startsWith("screenshot") || it == "screen" } -> What.SCREENSHOT
            w.any { it in setOf("photo", "photos", "picture", "pic", "pics", "image", "selfie", "snap") } -> What.PHOTO
            w.any { it in setOf("video", "videos", "clip", "recording", "movie") } -> What.VIDEO
            w.any { it in setOf("download", "downloaded") } -> What.DOWNLOAD
            else -> What.NAMED
        }
    }

    /** "the file called report.pdf" → "report.pdf"; null if nothing is left. */
    fun cleanName(obj: String): String? =
        obj.split(' ').dropWhile { it in filler }.filter { it.isNotEmpty() }.joinToString(" ")
            .removeSuffix(" file").trim().ifEmpty { null }

    /** Which of [devices] the phrase means: by name, or "laptop"/"pc" when there's exactly one. */
    fun findDevice(phrase: String, devices: List<String>): String? {
        val p = squash(phrase)
        if (p.isEmpty()) return null
        devices.firstOrNull { squash(it) == p }?.let { return it }
        devices.filter { val d = squash(it); d.isNotEmpty() && (p.contains(d) || d.contains(p) && p.length >= 3) }
            .takeIf { it.size == 1 }?.let { return it[0] }
        // Any word of a name ("desktop" for DESKTOP-NIKHIL, "office" for Office PC).
        val words = phrase.split(' ').filter { it.length >= 3 && it !in filler }
        devices.filter { name -> val parts = normalize(name).split(' ', '-', '_'); words.any { it in parts } }
            .takeIf { it.size == 1 }?.let { return it[0] }
        if (words.any { it in generic } && devices.size == 1) return devices[0]
        return null
    }

    private fun stripDevice(target: String, devices: List<String>): String {
        val parts = findDevice(target, devices)?.let { normalize(it).split(' ', '-', '_').toSet() } ?: emptySet()
        return target.split(' ').filter { it !in parts && it !in generic }.joinToString(" ")
    }

    /** Which of [labels] a spoken folder name means ("docs" → Documents). */
    fun findFolder(phrase: String?, labels: List<String>): String? {
        val p = squash(phrase ?: return null)
        if (p.isEmpty()) return null
        labels.firstOrNull { squash(it) == p }?.let { return it }
        labels.firstOrNull { squash(it).removeSuffix("s") == p.removeSuffix("s") }?.let { return it }
        return labels.filter { val l = squash(it); l.startsWith(p) || p.startsWith(l) }.takeIf { it.size == 1 }?.get(0)
    }

    private fun squash(s: String) = normalize(s).filter { it.isLetterOrDigit() }

    /** How well a file name matches what was said (0 = not at all). */
    fun nameScore(file: String, said: String): Int {
        val f = normalize(file)
        val s = normalize(said)
        if (f == s) return 100
        if (f.substringBeforeLast('.') == s.substringBeforeLast('.')) return 90
        val words = s.substringBeforeLast('.').split(' ', '-', '_').filter { it.length >= 2 }
        if (words.isEmpty()) return 0
        val hit = words.count { squash(f).contains(squash(it)) }
        return if (hit == words.size) 50 + hit else 0
    }
}
