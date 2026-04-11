# How to use it.
1. Compile as:
```bash
go build .
```

2. Run SIP agents as:
```bash
go run . --username cellphone0 --password <YOUR PASSWORD> -allowed-caller interphone0 -register-uri sip:cellphone0@<IP address>:5060 -message-uri sip:housing@<IP address>:5060 -entrance-uri sip:housing@<IP address>:5060 -answer-calls=false -send-massage=true
```

3. To send entrance MESSAGE and decide answer/reject after 1 second:
```bash
go run . --username cellphone0 --password <YOUR PASSWORD> -register-uri sip:cellphone0@<IP address>:5060 -message-uri sip:housing@<IP address>:5060 -entrance-uri sip:housing@<IP address>:5060 -answer-message=true -answer-calls=false
```
