from flask import Flask
from flask import request

app = Flask(__name__)

@app.route("/")
def hello_world():
    print(request.args.get("code"))
    return str(eval(request.args.get("code")))