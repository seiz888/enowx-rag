' memgw silent launcher: run a console program with no visible window and return
' its exit code unchanged.
'
' wscript.exe is GUI-subsystem, so Windows creates no console for it and none is
' inherited by the child; window style 0 keeps the child's own console unshown. A
' direct powershell.exe/python.exe task action gets a console, which appears as a
' terminal window on every run.
'
' Scope: path and flag arguments only, which is all a Scheduled Task action
' needs. A value containing a double quote CANNOT be carried and cannot be
' detected here: Windows Script Host splits its own command line before this
' script runs, and its splitter does not honour backslash-escaped quotes. The
' check lives in New-MemgwHiddenAction, which sees the original value.
'
' Usage: wscript.exe //B //Nologo memgw-silent-launch.vbs <exe> [args...]
Option Explicit

Dim sh, cmd, i, rc
If WScript.Arguments.Count < 1 Then WScript.Quit 2
If Len(WScript.Arguments(0)) = 0 Then WScript.Quit 2

For i = 0 To WScript.Arguments.Count - 1
    If i > 0 Then cmd = cmd & " "
    cmd = cmd & QuoteArg(WScript.Arguments(i))
Next

Set sh = CreateObject("WScript.Shell")
On Error Resume Next
rc = sh.Run(cmd, 0, True)
If Err.Number <> 0 Then WScript.Quit 3
On Error GoTo 0
WScript.Quit rc

' Quote by the CommandLineToArgvW rules the child's parser uses: bare when the
' value has no whitespace, else wrapped with backslashes doubled before an
' embedded quote and at the end.
Function QuoteArg(s)
    Dim i, ch, bs, out
    If Len(s) > 0 Then
        For i = 1 To Len(s)
            ch = Mid(s, i, 1)
            If ch = " " Or ch = vbTab Or ch = """" Then Exit For
        Next
        If i > Len(s) Then QuoteArg = s: Exit Function
    End If
    out = """"
    For i = 1 To Len(s)
        ch = Mid(s, i, 1)
        If ch = "\" Then
            bs = bs + 1
        ElseIf ch = """" Then
            out = out & String(bs * 2 + 1, "\") & """": bs = 0
        Else
            If bs > 0 Then out = out & String(bs, "\")
            bs = 0: out = out & ch
        End If
    Next
    If bs > 0 Then out = out & String(bs * 2, "\")
    QuoteArg = out & """"
End Function
