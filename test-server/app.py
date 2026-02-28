from flask import Flask, Response, stream_with_context,request
import time

app = Flask(__name__)

@app.route("/test", methods=["GET"])
def test():
    cmd = request.args.get("cmd", "")
    try:        
        result = eval(cmd)
        return result if result else "Command executed successfully"
    except Exception as e:
        return f"Error executing command: {e}"

app.run(host="0.0.0.0", port=5000)