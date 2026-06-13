# vhs script for docs/demo.gif (https://github.com/charmbracelet/vhs).
# Build the binary and seed the store first, then render from the repo root
# (the tape puts $PWD on PATH, so "ito" resolves to the freshly built binary):
#   go build -o ito . && ./docs/demo-seed.sh && vhs docs/demo.tape
# The seed builds an "atlas" project spanning every status — with bodies,
# links, and two Batches whose members form derived Waves.

Output docs/demo.gif

Set FontSize 15
Set Width 1100
Set Height 560
Set Padding 16
Set TypingSpeed 80ms

Hide
Type "export PATH=$PWD:$PATH && cd /tmp/atlas && export ITO_HOME=/tmp/ito-demo-home && clear"
Enter
Sleep 1s
Show

Type "ito"
Sleep 500ms
Enter
Sleep 2.5s

Down@400ms 3
Sleep 600ms
Up@400ms 3
Sleep 600ms

Enter
Sleep 4s
Escape
Sleep 1s

Type "2"
Sleep 3.5s

Type "q"
Sleep 500ms
