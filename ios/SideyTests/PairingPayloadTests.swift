import Testing
@testable import Sidey

@Suite struct PairingPayloadTests {
    @Test func fullJSONPayload() {
        let payload = PairingPayload.parse(#"{"v":1,"url":"http://100.1.2.3:8080","token":"abc123"}"#)
        #expect(payload?.url == "http://100.1.2.3:8080")
        #expect(payload?.token == "abc123")
    }

    @Test func bareTokenFallsBackToManualURL() {
        let payload = PairingPayload.parse("  abc123  ")
        #expect(payload?.url == nil)
        #expect(payload?.token == "abc123")
    }

    @Test(arguments: ["", "   ", "\n\t "])
    func emptyInputIsNil(input: String) {
        #expect(PairingPayload.parse(input) == nil)
    }

    @Test func jsonWithoutTokenTreatedAsOpaqueString() {
        // Documents current behavior: anything unparseable is passed through
        // as a bare token rather than rejected.
        let payload = PairingPayload.parse(#"{"v":1,"url":"http://x"}"#)
        #expect(payload?.token == #"{"v":1,"url":"http://x"}"#)
    }
}
