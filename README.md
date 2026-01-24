# How to use it.
Run SIP agents as:
```bash
go run . --username cellphone0 --password <YOUR PASSWORD> -allowed-caller interphone0 -register-uri sip:cellphone0@<IP address>:5060 -message-uri sip:housing@<IP address>:5060 -entrance-uri sip:housing@<IP address>:5060 -answer-calls=false -send-messages=true
```
