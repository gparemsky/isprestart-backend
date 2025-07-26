string = "'\nPinging 8.8.8.8 with 32 bytes of data:\nReply from 8.8.8.8: bytes=32 time=44ms TTL=107\n\nPing statistics for 8.8.8.8:\n    Packets: Sent = 1, Received = 1, Lost = 0 (0% loss),\nApproximate round trip times in milli-seconds:\n    Minimum = 44ms, Maximum = 44ms, Average = 44ms\n'"

for word in (string.split()):
    print(word)
    if "ms" in word:
        time = [int(charecter) for charecter in word if charecter.isdigit()]
        print(int(''.join(map(str,time))))
        break
