Output cli.gif

Require b2bk
Require kubectl

Set Shell bash
Set FontSize 16
Set Columns 120
Set Rows 34
Set Padding 20
Set Theme "Catppuccin Frappe"
Set TypingSpeed 60ms
Set Framerate 20
Set WaitTimeout 3m
Set WaitPattern /^\$ ?$/

Hide
Type "export KUBECONFIG=$PWD/../../bin/demo-b2bk-demo.kubeconfig PATH=$PWD/../../bin:$PATH PS1='$ ' && ../../hack/demo/demo.sh guard || exit" Enter
Type "{ [ ! -f /usr/share/bash-completion/bash_completion ] || . /usr/share/bash-completion/bash_completion; } && source <(b2bk completion bash) && clear" Enter
Show

Sleep 500ms
Type "b2bk g"
Sleep 300ms
Tab
Sleep 500ms
Type "-A"
Enter
Wait
Sleep 4s

Hide
Type "clear" Enter
Show
Type "b2bk status -n no"
Sleep 300ms
Tab
Sleep 500ms
Enter
Wait
Sleep 4s

Hide
Type "clear" Enter
Show
Type "b2bk backup -n notes --wait --follow"
Sleep 300ms
Enter
Wait
Sleep 3s

Hide
Type "clear" Enter
Show
Type "b2bk snapshots -n notes"
Sleep 300ms
Enter
Sleep 2s
Hide
Wait
Show
Sleep 3s

Hide
Type "clear" Enter
Show
Type "b2bk doctor -n git"
Sleep 300ms
Enter
Wait
Sleep 4s

Hide
Type "clear" Enter
Show
Type "b2bk restore notes -n notes --to-new-pvc notes-restore --yes"
Sleep 300ms
Enter
Sleep 2s
Hide
Wait
Show
Sleep 4s
