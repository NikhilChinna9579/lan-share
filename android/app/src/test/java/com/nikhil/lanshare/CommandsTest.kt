package com.nikhil.lanshare

import com.nikhil.lanshare.Commands.Command
import com.nikhil.lanshare.Commands.What
import org.junit.Assert.assertEquals
import org.junit.Test

class CommandsTest {
    private val one = listOf("DESKTOP-NIKHIL")
    private val two = listOf("DESKTOP-NIKHIL", "Office PC")

    private fun send(t: String, d: List<String> = one) = Commands.parse(t, d) as Command.Send

    @Test fun sendThisToMyLaptop() =
        assertEquals(Command.Send(What.THIS, null, "DESKTOP-NIKHIL", null), send("Send this to my laptop"))

    @Test fun sendWithFolder() =
        assertEquals(Command.Send(What.PHOTO, null, "DESKTOP-NIKHIL", "documents"), send("send the last photo to desktop in documents"))

    @Test fun sendWithFolderAfterDevice() =
        assertEquals(Command.Send(What.THIS, null, "DESKTOP-NIKHIL", "downloads"), send("send this file to my laptop's downloads folder"))

    @Test fun folderOnDevice() =
        assertEquals(Command.Send(What.SCREENSHOT, null, "Office PC", "downloads"), send("send my latest screenshot to downloads on the office pc", two))

    @Test fun namedFileSpoken() =
        assertEquals(Command.Send(What.NAMED, "report.pdf", "Office PC", null), send("can you share report dot pdf with office pc", two))

    @Test fun genericLaptopIsUnclearWithTwo() =
        assertEquals(Command.Send(What.VIDEO, null, null, null), send("send the last video to my laptop", two))

    @Test fun noTarget() = assertEquals(Command.Send(What.THIS, null, null, null), send("send this"))

    @Test fun getFrom() =
        assertEquals(Command.Get("budget.xlsx", "DESKTOP-NIKHIL"), Commands.parse("Get budget.xlsx from my laptop", one))

    @Test fun getNoDevice() =
        assertEquals(Command.Get("holiday photos", null), Commands.parse("download the holiday photos", two))

    @Test fun status() = assertEquals(Command.Status("Office PC"), Commands.parse("is the office pc online?", two))

    @Test fun nonsense() = assertEquals(Command.Unknown, Commands.parse("what's the weather", one))

    @Test fun folders() {
        val labels = listOf("Downloads", "Documents", "Desktop", "Work")
        assertEquals("Documents", Commands.findFolder("documents", labels))
        assertEquals("Downloads", Commands.findFolder("download", labels))
        assertEquals("Work", Commands.findFolder("work", labels))
        assertEquals(null, Commands.findFolder("d", labels))
    }

    @Test fun names() {
        assert(Commands.nameScore("Report.pdf", "report.pdf") == 100)
        assert(Commands.nameScore("Quarterly report 2026.pdf", "quarterly report") > 0)
        assert(Commands.nameScore("notes.txt", "budget") == 0)
    }
}
