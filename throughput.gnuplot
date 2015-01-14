set title "RobustIRC throughput"
set ylab "messages/s"
set xlab "time"
set xdata time
set timefmt "%s"
set format x "%H:%M:%S"

plot "throughput.data" using 1:2 with linespoints title "messages"
