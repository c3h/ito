# vhs script for docs/demo.gif (https://github.com/charmbracelet/vhs).
# Expects a seeded demo store: an ITO_HOME with an "atlas" project whose
# issues span every status, plus bodies and links — see the repo history
# for the seeding commands. Run with: vhs docs/demo.tape

Output docs/demo.gif

Set FontSize 15
Set Width 1100
Set Height 560
Set Padding 16
Set TypingSpeed 80ms

Hide
Type "cd /tmp/atlas && export ITO_HOME=/tmp/ito-demo-home && export PATH=/Users/gimigliano/Documents/ito:$PATH && clear"
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
