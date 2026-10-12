Output tui.gif

Require b2bk
Require kubectl

Set Shell bash
Set FontSize 16
Set Columns 120
Set Rows 34
Set Padding 20
Set Theme "Catppuccin Frappe"
Set TypingSpeed 75ms
Set Framerate 20
Set WaitTimeout 3m

Hide
Type "export KUBECONFIG=$PWD/../../bin/demo-b2bk-demo.kubeconfig PATH=$PWD/../../bin:$PATH PS1='$ ' && ../../hack/demo/demo.sh guard || exit" Enter
Type "clear" Enter
Show

Sleep 500ms
Type "b2bk -A"
Sleep 400ms
Enter
Wait+Screen /5 backups/
Sleep 3s

Down 2
Sleep 600ms
Enter
Wait+Screen /RECENT RUNS/
Sleep 3s
Escape
Sleep 600ms

Down
Sleep 600ms
Type "l"
Wait+Screen /%/
Sleep 6s
Escape
Sleep 600ms

Down
Sleep 600ms
Type "s"
Sleep 1500ms
Hide
Wait+Screen /kept by/
Show
Sleep 2s
Tab
Hide
Wait+Screen /tag db \[files\]/
Wait+Screen /kept by/
Show
Sleep 2s
Enter
Hide
Wait+Screen /up a folder/
Show
Sleep 2s
Right
Sleep 1500ms
Right
Sleep 1500ms
Left
Sleep 1500ms

Type "r"
Hide
Wait+Screen /A new claim/
Show
Sleep 3s
Enter
Hide
Wait+Screen /would be restored/
Show
Sleep 3s
Enter
Hide
Wait+Screen /✔ done|✖ failed/
Show
Sleep 4s

Escape
Sleep 400ms
Escape
Sleep 400ms
Escape
Sleep 600ms
Up 3
Sleep 600ms
Type "d"
Wait+Screen /most recent run/
Sleep 4s
Escape
Sleep 600ms

Type "?"
Sleep 3s
Escape
Sleep 600ms
Type "q"
Sleep 1s
